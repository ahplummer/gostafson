// Package evals runs golden test cases against a profile and model: each case
// is one turn in a fresh copy of a fixture directory, checked afterwards by
// what the model called, what it replied and what the files contain.
package evals

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/ahplummer/gostafson/internal/approve"
	"github.com/ahplummer/gostafson/internal/client"
	"github.com/ahplummer/gostafson/internal/config"
	"github.com/ahplummer/gostafson/internal/guard"
	"github.com/ahplummer/gostafson/internal/loop"
	"github.com/ahplummer/gostafson/internal/media"
	"github.com/ahplummer/gostafson/internal/router"
)

// Case is one golden test. Text checks ignore case.
type Case struct {
	Name    string `toml:"name"`
	Profile string `toml:"profile"`
	Input   string `toml:"input"`
	Fixture string `toml:"fixture"` // directory under the fixtures dir copied into a fresh root; empty: an empty root
	// Turns makes a multi-turn case: each message goes through the same
	// per-chat routing as the real chat (tags, sticky profiles), with the
	// history carried over. Profile and Input are then unused; the checks
	// apply after the last turn, to its reply.
	Turns        []string `toml:"turns"`
	WantProfiles []string `toml:"want_profiles"` // the profile each turn must go to
	// Images are files under the fixtures dir sent with the first message
	// (prepared like any transport's images, PRD D34).
	Images []string `toml:"images"`
	// Approve answers every approval question: "yes" approves, anything else denies.
	Approve string `toml:"approve"`
	// WantAsk, when set, requires (true) or forbids (false) an approval question.
	WantAsk *bool `toml:"want_ask"`

	WantTools   []string    `toml:"want_tools"`   // each must be called at least once
	ForbidTools []string    `toml:"forbid_tools"` // none may be called
	ReplyAll    []string    `toml:"reply_all"`    // the reply contains every one
	ReplyAny    []string    `toml:"reply_any"`    // the reply contains at least one
	ReplyNone   []string    `toml:"reply_none"`   // the reply contains none
	Files       []FileCheck `toml:"files"`
}

// FileCheck is a check on one file in the root after the turn.
type FileCheck struct {
	Path      string   `toml:"path"`
	Contains  []string `toml:"contains"`  // the file exists and contains each
	Absent    bool     `toml:"absent"`    // the file doesn't exist
	Unchanged bool     `toml:"unchanged"` // the file is as the fixture had it
}

// Load reads cases from a TOML file with [[case]] tables.
func Load(path string) ([]Case, error) {
	var f struct {
		Case []Case `toml:"case"`
	}
	if _, err := toml.DecodeFile(path, &f); err != nil {
		return nil, err
	}
	for i, c := range f.Case {
		if c.Name == "" || (len(c.Turns) == 0 && (c.Profile == "" || c.Input == "")) {
			return nil, fmt.Errorf("case %d: name and either profile and input, or turns, are required", i+1)
		}
		if len(c.WantProfiles) > 0 && len(c.WantProfiles) != len(c.Turns) {
			return nil, fmt.Errorf("case %s: want_profiles needs one entry per turn", c.Name)
		}
	}
	return f.Case, nil
}

// Result is the outcome of one case.
type Result struct {
	Case       string   `json:"case"`
	Profile    string   `json:"profile"`
	Profiles   []string `json:"profiles,omitempty"` // per turn, for multi-turn cases
	Model      string   `json:"model"`
	Pass       bool     `json:"pass"`
	Failures   []string `json:"failures,omitempty"`
	Err        string   `json:"error,omitempty"`
	Steps      int      `json:"steps"`
	TurnMS     int64    `json:"turn_ms"`
	StepMS     []int64  `json:"step_ms"` // each model call
	Prompt     int      `json:"prompt_tokens"`
	Completion int      `json:"completion_tokens"`
	Calls      []string `json:"calls,omitempty"` // "read_file index.html"
	// ToolErrors counts calls rejected for reasons other than an approval
	// answer: unknown tool, bad arguments, path outside the root, missing file.
	ToolErrors int      `json:"tool_errors"`
	Asked      []string `json:"asked,omitempty"`
	Reply      string   `json:"reply"`
}

// Env is what cases run against: the profiles (each with the model under
// test already set), a model client per local profile, and the limits.
type Env struct {
	Profiles map[string]*config.Profile
	Default  string
	Clients  map[string]loop.Chatter
	Fixtures string
	MaxSteps int
}

// Run runs one case in a fresh copy of its fixture.
func Run(ctx context.Context, env Env, cs Case) Result {
	r := Result{Case: cs.Name, Profile: cs.Profile}
	root, err := os.MkdirTemp("", "gostafson-eval-")
	if err != nil {
		r.Err = err.Error()
		return r
	}
	defer os.RemoveAll(root)
	before := map[string][]byte{}
	if cs.Fixture != "" {
		src := filepath.Join(env.Fixtures, cs.Fixture)
		if err := os.CopyFS(root, os.DirFS(src)); err != nil {
			r.Err = fmt.Sprintf("fixture: %v", err)
			return r
		}
		for _, fc := range cs.Files {
			if b, err := os.ReadFile(filepath.Join(src, fc.Path)); err == nil {
				before[fc.Path] = b
			}
		}
	}

	turns := cs.Turns
	route := func(text string) (string, string, error) { return cs.Profile, text, nil }
	if len(turns) == 0 {
		turns = []string{cs.Input}
	} else {
		names := make([]string, 0, len(env.Profiles))
		for n := range env.Profiles {
			names = append(names, n)
		}
		rt, err := router.New(names, env.Default)
		if err != nil {
			r.Err = err.Error()
			return r
		}
		sticky := router.NewSticky(rt, func(p string) bool { return env.Profiles[p].Sticks() })
		route = func(text string) (string, string, error) {
			rr, err := sticky.Route("eval", text)
			return rr.Profile, rr.Text, err
		}
	}

	ap := &answerer{yes: strings.EqualFold(cs.Approve, "yes")}
	guards := map[string]*guard.Guard{}
	defer func() {
		for _, g := range guards {
			g.Close()
		}
	}()
	var ims []media.Image
	for _, f := range cs.Images {
		data, err := os.ReadFile(filepath.Join(env.Fixtures, f))
		if err == nil {
			var im media.Image
			if im, err = media.Prepare(ctx, f, data); err == nil {
				ims = append(ims, im)
			}
		}
		if err != nil {
			r.Err = fmt.Sprintf("image %s: %v", f, err)
			return r
		}
	}
	var history []client.Message
	start := time.Now()
	for i, text := range turns {
		name, input, err := route(text)
		if err != nil {
			r.Err = err.Error()
			break
		}
		p, c := env.Profiles[name], env.Clients[name]
		if p == nil || c == nil {
			r.Err = fmt.Sprintf("profile %q is not available to evals (local profiles only)", name)
			break
		}
		r.Profiles = append(r.Profiles, name)
		r.Profile, r.Model = name, p.Model
		g, ok := guards[name]
		if !ok {
			dir := ""
			if len(p.Tools) > 0 {
				dir = root
			}
			if g, err = guard.New(name, p.Tools, dir, nil); err != nil {
				r.Err = err.Error()
				break
			}
			guards[name] = g
		}
		tools := &recorder{inner: g.ForTurn("eval", "", "", ap), ap: ap, r: &r}
		turn := loop.Turn{Profile: p, History: history, Input: input, MaxSteps: env.MaxSteps}
		if i == 0 {
			turn.Images = ims
		}
		res, err := loop.Run(ctx, &timer{inner: c, r: &r}, tools, turn)
		r.Steps += res.Steps
		r.Reply = res.Reply
		r.Prompt += res.Usage.PromptTokens
		r.Completion += res.Usage.CompletionTokens
		if i == 0 && len(ims) > 0 && len(res.New) > 0 {
			res.New[0].Images = ims // keep the image for follow-ups, as the chat does
		}
		history = append(history, res.New...)
		if err != nil {
			r.Err = err.Error()
			break
		}
	}
	r.TurnMS = time.Since(start).Milliseconds()
	r.Asked = ap.asked
	r.Failures = check(cs, r, root, before)
	r.Pass = r.Err == "" && len(r.Failures) == 0
	return r
}

func check(cs Case, r Result, root string, before map[string][]byte) []string {
	var fails []string
	failf := func(format string, a ...any) { fails = append(fails, fmt.Sprintf(format, a...)) }
	if len(cs.WantProfiles) > 0 && !slices.Equal(cs.WantProfiles, r.Profiles) {
		failf("turns went to %v, want %v", r.Profiles, cs.WantProfiles)
	}
	called := map[string]bool{}
	for _, c := range r.Calls {
		called[strings.TrimSuffix(strings.Fields(c)[0], ":")] = true // "run: ls -la" counts as run
	}
	for _, t := range cs.WantTools {
		if !called[t] {
			failf("didn't call %s", t)
		}
	}
	for _, t := range cs.ForbidTools {
		if called[t] {
			failf("called %s", t)
		}
	}
	if cs.WantAsk != nil && *cs.WantAsk != (len(r.Asked) > 0) {
		if *cs.WantAsk {
			failf("no approval question")
		} else {
			failf("asked for approval: %s", r.Asked[0])
		}
	}
	reply := strings.ToLower(r.Reply)
	for _, s := range cs.ReplyAll {
		if !strings.Contains(reply, strings.ToLower(s)) {
			failf("reply lacks %q", s)
		}
	}
	if len(cs.ReplyAny) > 0 && !slices.ContainsFunc(cs.ReplyAny, func(s string) bool { return strings.Contains(reply, strings.ToLower(s)) }) {
		failf("reply has none of %q", cs.ReplyAny)
	}
	for _, s := range cs.ReplyNone {
		if strings.Contains(reply, strings.ToLower(s)) {
			failf("reply contains %q", s)
		}
	}
	for _, fc := range cs.Files {
		b, err := os.ReadFile(filepath.Join(root, fc.Path))
		switch {
		case fc.Absent:
			if err == nil {
				failf("%s exists", fc.Path)
			}
			continue
		case err != nil:
			failf("%s missing", fc.Path)
			continue
		}
		if fc.Unchanged && string(b) != string(before[fc.Path]) {
			failf("%s changed", fc.Path)
		}
		low := strings.ToLower(string(b))
		for _, s := range fc.Contains {
			if !strings.Contains(low, strings.ToLower(s)) {
				failf("%s lacks %q", fc.Path, s)
			}
		}
	}
	return fails
}

// answerer gives the case's fixed answer to every approval question.
type answerer struct {
	yes   bool
	asked []string
}

func (a *answerer) Approve(_ context.Context, _, q string) approve.Result {
	a.asked = append(a.asked, q)
	if a.yes {
		return approve.Approved
	}
	return approve.Denied
}

// recorder notes each tool call and counts the ones that failed for a reason
// other than an approval answer.
type recorder struct {
	inner loop.Tools
	ap    *answerer
	r     *Result
}

func (t *recorder) Definitions() []client.Tool { return t.inner.Definitions() }

func (t *recorder) Call(ctx context.Context, name, args string) (string, error) {
	var a struct {
		Path    string   `json:"path"`
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	json.Unmarshal([]byte(args), &a)
	desc := name + " " + a.Path
	if name == "run" {
		desc = "run: " + strings.Join(append([]string{a.Command}, a.Args...), " ")
	}
	t.r.Calls = append(t.r.Calls, strings.TrimSpace(desc))
	asked := len(t.ap.asked)
	out, err := t.inner.Call(ctx, name, args)
	if err != nil && len(t.ap.asked) == asked {
		t.r.ToolErrors++
	}
	return out, err
}

// timer records how long each model call takes.
type timer struct {
	inner loop.Chatter
	r     *Result
}

func (t *timer) Chat(ctx context.Context, req client.Request) (client.Reply, error) {
	start := time.Now()
	rep, err := t.inner.Chat(ctx, req)
	t.r.StepMS = append(t.r.StepMS, time.Since(start).Milliseconds())
	return rep, err
}
