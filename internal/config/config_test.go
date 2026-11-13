package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, dir, name, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	return p
}

const minimal = `
[sessions]
path = "var/sessions.db"

[profiles.default]
base_url = "http://localhost:11434/v1"
model = "qwen3.5:9b"
think_toggle = true
`

func TestLoadDefaultsAndPaths(t *testing.T) {
	dir := t.TempDir()
	c, err := Load(write(t, dir, "c.toml", minimal, 0o644))
	if err != nil {
		t.Fatal(err)
	}
	if c.DefaultProfile != "default" || c.Loop.MaxSteps != 8 || c.Loop.TurnTimeout != 120*time.Second || c.Sessions.MaxMessages != 40 {
		t.Errorf("defaults not applied: %+v", c)
	}
	if want := filepath.Join(dir, "var/sessions.db"); c.Sessions.Path != want {
		t.Errorf("sessions path = %q, want %q", c.Sessions.Path, want)
	}
	p := c.Profiles["default"]
	if p.Name != "default" || !p.ThinkToggle {
		t.Errorf("profile = %+v", p)
	}
}

func TestLoadDuration(t *testing.T) {
	dir := t.TempDir()
	c, err := Load(write(t, dir, "c.toml", minimal+"\n[loop]\nturn_timeout = \"45s\"\n", 0o644))
	if err != nil {
		t.Fatal(err)
	}
	if c.Loop.TurnTimeout != 45*time.Second {
		t.Errorf("turn_timeout = %v", c.Loop.TurnTimeout)
	}
}

func TestLoadRejects(t *testing.T) {
	tests := map[string]struct{ body, want string }{
		"unknown key":     {minimal + "bogus = 1\n", "unknown keys"},
		"missing default": {strings.Replace(minimal, "profiles.default", "profiles.code", 1), `default_profile "default"`},
		"bad name":        {minimal + "[profiles.Code]\nbase_url = \"x\"\nmodel = \"y\"\n", `profile name "Code"`},
		"no model":        {minimal + "[profiles.code]\nbase_url = \"x\"\n", "profiles.code.model is required"},
		"key no secrets":  {minimal + "[profiles.cloud]\nbase_url = \"x\"\nmodel = \"y\"\napi_key = \"openrouter\"\n", "secrets_file is not"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Load(write(t, t.TempDir(), "c.toml", tt.body, 0o644))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestEnvKey(t *testing.T) {
	body := minimal + `
[profiles.cloud]
base_url = "https://openrouter.ai/api/v1"
model = "some/model"
api_key_env = "GOSTAFSON_TEST_KEY"
`
	t.Run("set", func(t *testing.T) {
		t.Setenv("GOSTAFSON_TEST_KEY", "sk-env")
		c, err := Load(write(t, t.TempDir(), "c.toml", body, 0o644))
		if err != nil {
			t.Fatal(err)
		}
		if p := c.Profiles["cloud"]; p.ResolvedAPIKey != "sk-env" || p.MissingKey != "" {
			t.Errorf("profile = %+v", p)
		}
	})
	t.Run("unset loads but marks missing", func(t *testing.T) {
		t.Setenv("GOSTAFSON_TEST_KEY", "")
		c, err := Load(write(t, t.TempDir(), "c.toml", body, 0o644))
		if err != nil {
			t.Fatal(err)
		}
		if p := c.Profiles["cloud"]; p.ResolvedAPIKey != "" || !strings.Contains(p.MissingKey, "GOSTAFSON_TEST_KEY") {
			t.Errorf("profile = %+v", p)
		}
		if c.Profiles["default"].MissingKey != "" {
			t.Error("local profile should not be affected")
		}
	})
	t.Run("both sources is an error", func(t *testing.T) {
		both := `secrets_file = "s.toml"` + "\n" + body + `api_key = "openrouter"` + "\n"
		_, err := Load(write(t, t.TempDir(), "c.toml", both, 0o644))
		if err == nil || !strings.Contains(err.Error(), "not both") {
			t.Errorf("err = %v", err)
		}
	})
}

func TestSecrets(t *testing.T) {
	body := `secrets_file = "secrets.toml"` + "\n" + minimal + `
[profiles.cloud]
base_url = "https://openrouter.ai/api/v1"
model = "some/model"
api_key = "openrouter"
`
	t.Run("loaded", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "secrets.toml", `openrouter = "sk-test"`, 0o600)
		c, err := Load(write(t, dir, "c.toml", body, 0o644))
		if err != nil {
			t.Fatal(err)
		}
		if c.Profiles["cloud"].ResolvedAPIKey != "sk-test" {
			t.Errorf("key not resolved")
		}
	})
	t.Run("world readable", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "secrets.toml", `openrouter = "sk-test"`, 0o644)
		_, err := Load(write(t, dir, "c.toml", body, 0o644))
		if err == nil || !strings.Contains(err.Error(), "chmod 600") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("missing key", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "secrets.toml", `other = "x"`, 0o600)
		_, err := Load(write(t, dir, "c.toml", body, 0o644))
		if err == nil || !strings.Contains(err.Error(), `"openrouter" not found`) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestToolsAndRoot(t *testing.T) {
	dir := t.TempDir()
	c, err := Load(write(t, dir, "c.toml", minimal+`
[profiles.code]
base_url = "http://127.0.0.1:11434/v1"
model = "m"
tools = ["read_file"]
root = "work/code"
`, 0o644))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "work/code"); c.Profiles["code"].Root != want {
		t.Errorf("root = %q, want %q", c.Profiles["code"].Root, want)
	}
	if want := filepath.Join(dir, "var/audit.jsonl"); c.Audit.Path != want {
		t.Errorf("audit path = %q, want %q", c.Audit.Path, want)
	}

	_, err = Load(write(t, dir, "noroot.toml", minimal+`
[profiles.code]
base_url = "http://localhost:11434/v1"
model = "m"
tools = ["read_file"]
`, 0o644))
	if err == nil || !strings.Contains(err.Error(), "root is not") {
		t.Errorf("tools without root: %v", err)
	}
}

// PRD D4: a profile on a remote server gets no file tools unless the config
// says so explicitly.
func TestRemoteProfileFileTools(t *testing.T) {
	dir := t.TempDir()
	cloud := minimal + `
[profiles.cloud]
base_url = "https://openrouter.ai/api/v1"
model = "m"
tools = ["read_file"]
root = "work"
`
	if _, err := Load(write(t, dir, "a.toml", cloud, 0o644)); err == nil || !strings.Contains(err.Error(), "allow_remote_file_tools") {
		t.Errorf("remote profile with tools: %v", err)
	}
	if _, err := Load(write(t, dir, "b.toml", cloud+"allow_remote_file_tools = true\n", 0o644)); err != nil {
		t.Errorf("explicit override: %v", err)
	}
}

func TestIsLocal(t *testing.T) {
	for u, want := range map[string]bool{
		"http://localhost:11434/v1":    true,
		"http://127.0.0.1:1234/v1":     true,
		"http://[::1]:11434/v1":        true,
		"https://openrouter.ai/api/v1": false,
		"http://192.168.1.5:11434/v1":  false,
		"http://localhost.evil.com/v1": false,
		"not a url":                    false,
	} {
		if got := IsLocal(u); got != want {
			t.Errorf("IsLocal(%q) = %v", u, got)
		}
	}
}

func TestOpenRouterProviderDefaults(t *testing.T) {
	dir := t.TempDir()
	c, err := Load(write(t, dir, "c.toml", minimal+`
[profiles.cloud]
base_url = "https://openrouter.ai/api/v1"
model = "m"

[profiles.zdr]
base_url = "https://openrouter.ai/api/v1"
model = "m"
provider = { zdr = true, data_collection = "allow" }

[profiles.other]
base_url = "https://api.example.com/v1"
model = "m"
`, 0o644))
	if err != nil {
		t.Fatal(err)
	}
	if v := c.Profiles["cloud"].Provider["data_collection"]; v != "deny" {
		t.Errorf("cloud data_collection = %v, want deny", v)
	}
	if p := c.Profiles["zdr"].Provider; p["data_collection"] != "allow" || p["zdr"] != true {
		t.Errorf("explicit provider overridden: %v", p)
	}
	if p := c.Profiles["other"].Provider; p != nil {
		t.Errorf("non-OpenRouter profile got provider %v", p)
	}
	if !c.Profiles["cloud"].Remote() || c.Profiles["default"].Remote() {
		t.Error("Remote() wrong")
	}
}

// PRD D4: untagged messages never go off the host, so the default profile
// must be local.
func TestDefaultProfileMustBeLocal(t *testing.T) {
	dir := t.TempDir()
	_, err := Load(write(t, dir, "c.toml", `default_profile = "cloud"
[sessions]
path = "s.db"
[profiles.cloud]
base_url = "https://openrouter.ai/api/v1"
model = "m"
`, 0o644))
	if err == nil || !strings.Contains(err.Error(), "must be a local server") {
		t.Errorf("remote default: %v", err)
	}
}

func TestWebConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GOSTAFSON_TEST_WEB_TOKEN", "tok123")
	c, err := Load(write(t, dir, "ok.toml", minimal+`
[web]
listen = "100.101.102.103:8484"
token_env = "GOSTAFSON_TEST_WEB_TOKEN"
`, 0o644))
	if err != nil {
		t.Fatal(err)
	}
	if c.Web.ResolvedToken != "tok123" || !c.CLI.On() {
		t.Errorf("web = %+v, cli on = %v", c.Web, c.CLI.On())
	}

	for name, body := range map[string]string{
		"all interfaces": "[web]\nlisten = \":8484\"\ntoken_env = \"GOSTAFSON_TEST_WEB_TOKEN\"\n",
		"zero address":   "[web]\nlisten = \"0.0.0.0:8484\"\ntoken_env = \"GOSTAFSON_TEST_WEB_TOKEN\"\n",
		"LAN address":    "[web]\nlisten = \"192.168.1.20:8484\"\ntoken_env = \"GOSTAFSON_TEST_WEB_TOKEN\"\n",
		"no token":       "[web]\nlisten = \"127.0.0.1:8484\"\n",
		"unset env":      "[web]\nlisten = \"127.0.0.1:8484\"\ntoken_env = \"GOSTAFSON_TEST_UNSET\"\n",
	} {
		if _, err := Load(write(t, dir, "bad.toml", minimal+body, 0o644)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestPrivateListen(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8484":           true,
		"[::1]:8484":               true,
		"100.64.0.1:8484":          true,
		"100.127.255.254:80":       true,
		"[fd7a:115c:a1e0::1]:8484": true,
		"100.128.0.1:8484":         false,
		"0.0.0.0:8484":             false,
		":8484":                    false,
		"localhost:8484":           false,
		"10.0.0.5:8484":            false,
		"127.0.0.1":                false,
	} {
		if got := PrivateListen(addr); got != want {
			t.Errorf("PrivateListen(%q) = %v", addr, got)
		}
	}
}

func TestCLIDisabled(t *testing.T) {
	c, err := Load(write(t, t.TempDir(), "c.toml", minimal+"[cli]\nenabled = false\n", 0o644))
	if err != nil || c.CLI.On() {
		t.Errorf("cli on = %v, err %v", c.CLI.On(), err)
	}
}

func TestTelegramConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GOSTAFSON_TEST_TG", "123:abc")
	c, err := Load(write(t, dir, "ok.toml", minimal+"[telegram]\ntoken_env = \"GOSTAFSON_TEST_TG\"\nallowed_users = [42]\n", 0o644))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Telegram.On() || c.Telegram.ResolvedToken != "123:abc" || c.Telegram.APIBase != "https://api.telegram.org" {
		t.Errorf("telegram = %+v", c.Telegram)
	}
	if _, err := Load(write(t, dir, "bad.toml", minimal+"[telegram]\ntoken_env = \"GOSTAFSON_TEST_TG\"\n", 0o644)); err == nil || !strings.Contains(err.Error(), "allowed_users") {
		t.Errorf("no allowed_users: %v", err)
	}
	if c, _ := Load(write(t, dir, "off.toml", minimal, 0o644)); c.Telegram.On() {
		t.Error("telegram on without config")
	}
}

// The host template must stay loadable once its <placeholders> are filled in.
func TestHostExampleLoads(t *testing.T) {
	raw, err := os.ReadFile("../../config.host.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	secrets := write(t, dir, "secrets.toml", "openrouter = \"k\"\ntelegram_token = \"t\"\nweb_token = \"w\"\n", 0o600)
	body := strings.NewReplacer(
		"<100.x.y.z>", "100.64.0.1",
		"<your Telegram user ID>", "42",
		"<your npub>", "npub10elfcs4fr0l0r8af98jlmgdh9c8tcxjvz9qkw038js35mp4dma8qzvjptg",
		"<provider/model>", "p/m",
		"<anthropic/model>", "anthropic/m",
		"/Users/agent/secrets.toml", secrets,
	).Replace(string(raw))
	if strings.Contains(body, "<") {
		t.Fatalf("unreplaced placeholder in host example")
	}
	c, err := Load(write(t, dir, "host.toml", body, 0o644))
	if err != nil {
		t.Fatal(err)
	}
	if c.CLI.On() || c.Web.ResolvedToken != "w" || c.Telegram.ResolvedToken != "t" || c.Profiles["sonnet"].ResolvedAPIKey != "k" || !c.Profiles["opus"].Sticks() || c.Profiles["sonnet"].Sticks() || !c.Profiles["code"].Sticks() {
		t.Errorf("host config = cli %v, web %q, telegram %q", c.CLI.On(), c.Web.ResolvedToken, c.Telegram.ResolvedToken)
	}
	if c.Profiles["code"].Root != "/Users/agent/work/napps" {
		t.Errorf("code root = %q", c.Profiles["code"].Root)
	}
}
