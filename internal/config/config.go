// Package config loads gostafson's TOML configuration and secrets file.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the whole configuration file.
type Config struct {
	DefaultProfile string              `toml:"default_profile"`
	SecretsFile    string              `toml:"secrets_file"`
	Sessions       Sessions            `toml:"sessions"`
	Audit          Audit               `toml:"audit"`
	Approval       Approval            `toml:"approval"`
	CLI            CLI                 `toml:"cli"`
	Web            Web                 `toml:"web"`
	Telegram       Telegram            `toml:"telegram"`
	WhiteNoise     WhiteNoise          `toml:"whitenoise"`
	Loop           Loop                `toml:"loop"`
	Projects       Projects            `toml:"projects"`
	Profiles       map[string]*Profile `toml:"profiles"`
}

// Projects configures project directories and their search (PRD D37).
type Projects struct {
	// Dir holds one directory per project. Default: "projects" beside the
	// sessions database.
	Dir string `toml:"dir"`
	// EmbedModel is the Ollama model for meaning search, on the default
	// profile's server. "none" turns meaning search off (keyword only).
	EmbedModel string `toml:"embed_model"`
}

// Sessions configures conversation history storage.
type Sessions struct {
	Path        string `toml:"path"`         // SQLite file; relative paths are relative to the config file
	MaxMessages int    `toml:"max_messages"` // history messages sent with each turn
}

// Audit configures the JSONL audit log.
type Audit struct {
	Path string `toml:"path"` // relative paths are relative to the config file
}

// Approval configures yes/no prompts for risky tool calls.
type Approval struct {
	Timeout time.Duration `toml:"timeout"` // no answer within this time means no
}

// CLI configures the terminal transport.
type CLI struct {
	Enabled *bool `toml:"enabled"` // default true; set false for the background service
}

// On reports whether the CLI transport runs.
func (c CLI) On() bool { return c.Enabled == nil || *c.Enabled }

// Web configures the browser transport (a page served on the tailnet).
type Web struct {
	Listen   string `toml:"listen"`    // host:port; host must be loopback or a Tailscale address. Empty disables it
	Token    string `toml:"token"`     // name of the login token in the secrets file
	TokenEnv string `toml:"token_env"` // or: environment variable holding it

	ResolvedToken string `toml:"-"`
}

// Telegram configures the bot transport. Bot chats are not end-to-end
// encrypted: Telegram's servers see messages and replies.
type Telegram struct {
	Token        string  `toml:"token"`         // name of the bot token in the secrets file
	TokenEnv     string  `toml:"token_env"`     // or: environment variable holding it
	AllowedUsers []int64 `toml:"allowed_users"` // Telegram user IDs answered; required
	APIBase      string  `toml:"api_base"`      // default https://api.telegram.org

	ResolvedToken string `toml:"-"`
}

// On reports whether the Telegram transport is configured.
func (t Telegram) On() bool { return t.Token != "" || t.TokenEnv != "" }

// WhiteNoise configures the White Noise / Marmot transport, which talks to a
// local wn-agent over its Unix control socket.
type WhiteNoise struct {
	Socket         string   `toml:"socket"`          // wn-agent control socket; empty disables it
	AuthTokenFile  string   `toml:"auth_token_file"` // if wn-agent runs with --auth-token-file
	AccountIDHex   string   `toml:"account_id_hex"`  // optional when wn-agent has one signing account
	AllowedSenders []string `toml:"allowed_senders"` // npubs or hex account IDs answered; required

	ResolvedAuthToken string `toml:"-"`
}

// Loop configures the agent loop limits.
type Loop struct {
	MaxSteps    int           `toml:"max_steps"`
	TurnTimeout time.Duration `toml:"turn_timeout"`
}

// Profile is one backend: endpoint, model and behavior. Its name is also its @tag.
type Profile struct {
	Name         string   `toml:"-"`
	BaseURL      string   `toml:"base_url"`
	Model        string   `toml:"model"`
	APIKey       string   `toml:"api_key"`      // name of a key in the secrets file, not the key itself
	APIKeyEnv    string   `toml:"api_key_env"`  // or: name of an environment variable holding the key
	ThinkToggle  bool     `toml:"think_toggle"` // send reasoning_effort=none unless the message has think=true
	Temperature  *float64 `toml:"temperature"`
	MaxTokens    int      `toml:"max_tokens"`
	SystemPrompt string   `toml:"system_prompt"`
	Tools        []string `toml:"tools"` // tool names this profile may call; empty means chat only
	Root         string   `toml:"root"`  // directory the file tools are confined to
	// RemoteFileTools must be set to give file tools to a profile whose
	// base_url isn't on this machine (PRD D4: cloud profiles get none by default).
	RemoteFileTools bool `toml:"allow_remote_file_tools"`
	// Provider is sent as OpenRouter's "provider" object. For openrouter.ai
	// profiles, data_collection defaults to "deny".
	Provider map[string]any `toml:"provider"`
	// Plugins is sent as OpenRouter's "plugins" array, e.g.
	// [{ id = "web", engine = "exa", max_results = 5 }] for web search.
	Plugins []map[string]any `toml:"plugins"`
	// Sticky lets a remote profile's tag stick for the chat, like a local
	// profile's (PRD D33): untagged messages then go to the cloud until another
	// tag, @default or /reset, and every reply says which profile answered.
	// Local profiles always stick; this only matters for remote ones.
	Sticky bool `toml:"sticky"`

	ResolvedAPIKey string `toml:"-"`
	// MissingKey is set when the profile needs a key that isn't available, e.g.
	// an unset environment variable. The profile loads but can't be used.
	MissingKey string `toml:"-"`
}

var profileName = regexp.MustCompile(`^[a-z0-9_-]+$`)

// Load reads the config file at path, applies defaults, resolves relative
// paths against the config file's directory, loads referenced secrets and
// validates the result.
func Load(path string) (*Config, error) {
	var c Config
	md, err := toml.DecodeFile(path, &c)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return nil, fmt.Errorf("config %s: unknown keys: %v", path, undecoded)
	}

	base := filepath.Dir(path)
	c.applyDefaults()
	c.Sessions.Path = resolve(base, c.Sessions.Path)
	c.SecretsFile = resolve(base, c.SecretsFile)
	c.Audit.Path = resolve(base, c.Audit.Path)
	if c.Projects.Dir == "" {
		c.Projects.Dir = filepath.Join(filepath.Dir(c.Sessions.Path), "projects")
	} else {
		c.Projects.Dir = resolve(base, expandHome(c.Projects.Dir))
	}
	c.WhiteNoise.Socket = resolve(base, expandHome(c.WhiteNoise.Socket))
	c.WhiteNoise.AuthTokenFile = resolve(base, expandHome(c.WhiteNoise.AuthTokenFile))
	for _, p := range c.Profiles {
		if p != nil {
			p.Root = resolve(base, p.Root)
		}
	}

	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if err := c.loadSecrets(); err != nil {
		return nil, err
	}
	if f := c.WhiteNoise.AuthTokenFile; f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("whitenoise.auth_token_file: %w", err)
		}
		c.WhiteNoise.ResolvedAuthToken = strings.TrimSpace(string(b))
	}
	if err := c.loadEnvKeys(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &c, nil
}

// ProfileNames returns the profile names, sorted.
func (c *Config) ProfileNames() []string {
	names := make([]string, 0, len(c.Profiles))
	for n := range c.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func (c *Config) applyDefaults() {
	if c.DefaultProfile == "" {
		c.DefaultProfile = "default"
	}
	if c.Sessions.MaxMessages == 0 {
		c.Sessions.MaxMessages = 40
	}
	if c.Audit.Path == "" {
		c.Audit.Path = "var/audit.jsonl"
	}
	if c.Telegram.APIBase == "" {
		c.Telegram.APIBase = "https://api.telegram.org"
	}
	if c.Approval.Timeout == 0 {
		c.Approval.Timeout = 60 * time.Second
	}
	if c.Loop.MaxSteps == 0 {
		c.Loop.MaxSteps = 8
	}
	if c.Loop.TurnTimeout == 0 {
		c.Loop.TurnTimeout = 120 * time.Second
	}
	if c.Projects.EmbedModel == "" {
		c.Projects.EmbedModel = "nomic-embed-text"
	}
	for name, p := range c.Profiles {
		if p == nil {
			continue
		}
		p.Name = name
		if u, err := url.Parse(p.BaseURL); err == nil && u.Hostname() == "openrouter.ai" {
			if p.Provider == nil {
				p.Provider = map[string]any{}
			}
			if _, set := p.Provider["data_collection"]; !set {
				p.Provider["data_collection"] = "deny"
			}
		}
	}
}

// Remote reports whether the profile's server is off this machine. Remote
// profiles get text-only history and no file tools by default.
func (p *Profile) Remote() bool { return !IsLocal(p.BaseURL) }

// Sticks reports whether the profile's tag sticks for the chat.
func (p *Profile) Sticks() bool { return !p.Remote() || p.Sticky }

func (c *Config) validate() error {
	var errs []error
	if c.Sessions.Path == "" {
		errs = append(errs, errors.New("sessions.path is required"))
	}
	if c.Sessions.MaxMessages < 0 || c.Loop.MaxSteps < 0 || c.Loop.TurnTimeout < 0 || c.Approval.Timeout < 0 {
		errs = append(errs, errors.New("limits must not be negative"))
	}
	if len(c.Profiles) == 0 {
		errs = append(errs, errors.New("at least one [profiles.<name>] is required"))
	}
	if p, ok := c.Profiles[c.DefaultProfile]; !ok {
		errs = append(errs, fmt.Errorf("default_profile %q has no [profiles.%s] section", c.DefaultProfile, c.DefaultProfile))
	} else if p != nil && p.Remote() {
		// Untagged messages go to the default profile; data leaves the host
		// only when the owner tags a message (PRD D4).
		errs = append(errs, fmt.Errorf("default_profile %q points at %s; the default must be a local server", c.DefaultProfile, p.BaseURL))
	}
	for _, name := range c.ProfileNames() {
		p := c.Profiles[name]
		if !profileName.MatchString(name) {
			errs = append(errs, fmt.Errorf("profile name %q: use lowercase letters, digits, - or _", name))
		}
		if p == nil {
			continue
		}
		if p.BaseURL == "" {
			errs = append(errs, fmt.Errorf("profiles.%s.base_url is required", name))
		}
		if p.Model == "" {
			errs = append(errs, fmt.Errorf("profiles.%s.model is required", name))
		}
		if p.APIKey != "" && p.APIKeyEnv != "" {
			errs = append(errs, fmt.Errorf("profiles.%s: set api_key or api_key_env, not both", name))
		}
		if len(p.Tools) > 0 && p.Root == "" {
			errs = append(errs, fmt.Errorf("profiles.%s: tools are set but root is not", name))
		}
		if len(p.Tools) > 0 && !IsLocal(p.BaseURL) && !p.RemoteFileTools {
			errs = append(errs, fmt.Errorf("profiles.%s: %s is not on this machine, so it gets no file tools; set allow_remote_file_tools = true to override", name, p.BaseURL))
		}
		if p.APIKey != "" && c.SecretsFile == "" {
			errs = append(errs, fmt.Errorf("profiles.%s.api_key is set but secrets_file is not", name))
		}
	}
	if c.Web.Listen != "" {
		if !PrivateListen(c.Web.Listen) {
			errs = append(errs, fmt.Errorf("web.listen %q: use a loopback or Tailscale IP with a port (e.g. 127.0.0.1:8484 or 100.x.y.z:8484), not all interfaces or the LAN", c.Web.Listen))
		}
		if (c.Web.Token == "") == (c.Web.TokenEnv == "") {
			errs = append(errs, errors.New("web: set exactly one of token or token_env"))
		}
	}
	if c.WhiteNoise.Socket != "" && len(c.WhiteNoise.AllowedSenders) == 0 {
		errs = append(errs, errors.New("whitenoise.allowed_senders is required: your npub"))
	}
	if c.Telegram.On() {
		if c.Telegram.Token != "" && c.Telegram.TokenEnv != "" {
			errs = append(errs, errors.New("telegram: set token or token_env, not both"))
		}
		if len(c.Telegram.AllowedUsers) == 0 {
			errs = append(errs, errors.New("telegram.allowed_users is required: your numeric Telegram user ID (message @userinfobot to get it)"))
		}
	}
	return errors.Join(errs...)
}

// loadSecrets reads the secrets file (a flat TOML table of name = "value")
// when anything refers to it, and resolves profile API keys and transport
// tokens. The file must not be readable by group or others.
func (c *Config) loadSecrets() error {
	needed := c.Web.Token != "" || c.Telegram.Token != ""
	for _, p := range c.Profiles {
		if p.APIKey != "" {
			needed = true
		}
	}
	if !needed {
		return nil
	}
	if c.SecretsFile == "" {
		return errors.New("a secret is referenced by name but secrets_file is not set")
	}

	info, err := os.Stat(c.SecretsFile)
	if err != nil {
		return fmt.Errorf("secrets file: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("secrets file %s has mode %v; run: chmod 600 %s", c.SecretsFile, info.Mode().Perm(), c.SecretsFile)
	}
	var secrets map[string]string
	if _, err := toml.DecodeFile(c.SecretsFile, &secrets); err != nil {
		return fmt.Errorf("read secrets file: %w", err)
	}
	lookup := func(field, name string) (string, error) {
		v, ok := secrets[name]
		if !ok || v == "" {
			return "", fmt.Errorf("%s: %q not found in secrets file", field, name)
		}
		return v, nil
	}
	for _, name := range c.ProfileNames() {
		p := c.Profiles[name]
		if p.APIKey == "" {
			continue
		}
		if p.ResolvedAPIKey, err = lookup("profiles."+name+".api_key", p.APIKey); err != nil {
			return err
		}
	}
	if c.Web.Token != "" {
		if c.Web.ResolvedToken, err = lookup("web.token", c.Web.Token); err != nil {
			return err
		}
	}
	if c.Telegram.Token != "" {
		if c.Telegram.ResolvedToken, err = lookup("telegram.token", c.Telegram.Token); err != nil {
			return err
		}
	}
	return nil
}

// loadEnvKeys fills keys and tokens from environment variables. An unset
// variable doesn't fail the load for a profile, so local profiles still work;
// the profile is marked with MissingKey instead. A transport with an unset
// token variable fails the load: it would otherwise run without auth.
func (c *Config) loadEnvKeys() error {
	for _, p := range c.Profiles {
		if p.APIKeyEnv == "" {
			continue
		}
		if v := os.Getenv(p.APIKeyEnv); v != "" {
			p.ResolvedAPIKey = v
		} else {
			p.MissingKey = fmt.Sprintf("environment variable %s is not set", p.APIKeyEnv)
		}
	}
	if c.Web.TokenEnv != "" {
		if c.Web.ResolvedToken = os.Getenv(c.Web.TokenEnv); c.Web.ResolvedToken == "" {
			return fmt.Errorf("web.token_env: environment variable %s is not set", c.Web.TokenEnv)
		}
	}
	if c.Telegram.TokenEnv != "" {
		if c.Telegram.ResolvedToken = os.Getenv(c.Telegram.TokenEnv); c.Telegram.ResolvedToken == "" {
			return fmt.Errorf("telegram.token_env: environment variable %s is not set", c.Telegram.TokenEnv)
		}
	}
	return nil
}

// tailnetV4 and tailnetV6 are the address ranges Tailscale assigns.
var (
	_, tailnetV4, _ = net.ParseCIDR("100.64.0.0/10")
	_, tailnetV6, _ = net.ParseCIDR("fd7a:115c:a1e0::/48")
)

// PrivateListen reports whether a host:port listen address is limited to this
// machine or the tailnet: a literal loopback or Tailscale IP. Empty hosts,
// 0.0.0.0, LAN addresses and names are rejected.
func PrivateListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || tailnetV4.Contains(ip) || tailnetV6.Contains(ip))
}

// IsLocal reports whether a base URL points at this machine (localhost or a
// loopback address). Remote URLs are treated as cloud.
func IsLocal(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	h := u.Hostname()
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// expandHome turns a leading "~/" into the user's home directory.
func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return p
}

func resolve(base, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}
