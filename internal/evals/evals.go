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
)

// Case is one golden test. Text checks ignore case.
type Case struct {
	Name    string `toml:"name"`
	Profile string `toml:"profile"`
	Input   string `toml:"input"`
	Fixture string `toml:"fixture"` // directory under the fixtures dir copied into a fresh root; empty: an empty root
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
		if c.Name == "" || c.Profile == "" || c.Input == "" {
			return nil, fmt.Errorf("case %d: name, profile and input are required", i+1)
		}
	}
	return f.Case, nil
}

// Result is the outcome of one case.
type Result struct {
	Case       string   `json:"case"`
	Profile    string   `json:"profile"`
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

// Run runs one case with profile p (its Model already set to the model under
// test). fixtures is the directory holding the fixture directories.
func Run(ctx context.Context, c loop.Chatter, p *config.Profile, cs Case, fixtures string, maxSteps int) Result {
	r := Result{Case: cs.Name, Profile: p.Name, Model: p.Model}
	root, err := os.MkdirTemp("", "gostafson-eval-")
	if err != nil {
		r.Err = err.Error()
		return r
	}
	defer os.RemoveAll(root)
	before := map[string][]byte{}
	if cs.Fixture != "" {
		src := filepath.Join(fixtures, cs.Fixture)
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

	dir := ""
	if len(p.Tools) > 0 {
		dir = root
	}
	g, err := guard.New(p.Name, p.Tools, dir, nil)
	if err != nil {
		r.Err = err.Error()
		return r
	}
	defer g.Close()
	ap := &answerer{yes: strings.EqualFold(cs.Approve, "yes")}
	tools := &recorder{inner: g.ForTurn("eval", "", "", ap), ap: ap, r: &r}
	timed := &timer{inner: c, r: &r}

	start := time.Now()
	res, err := loop.Run(ctx, timed, tools, loop.Turn{Profile: p, Input: cs.Input, MaxSteps: maxSteps})
	r.TurnMS = time.Since(start).Milliseconds()
	r.Steps, r.Reply = res.Steps, res.Reply
	r.Prompt, r.Completion = res.Usage.PromptTokens, res.Usage.CompletionTokens
	r.Asked = ap.asked
	if err != nil {
		r.Err = err.Error()
	}
	r.Failures = check(cs, r, root, before)
	r.Pass = r.Err == "" && len(r.Failures) == 0
	return r
}

func check(cs Case, r Result, root string, before map[string][]byte) []string {
	var fails []string
	failf := func(format string, a ...any) { fails = append(fails, fmt.Sprintf(format, a...)) }
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
