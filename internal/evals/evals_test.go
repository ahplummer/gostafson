package evals

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ahplummer/gostafson/internal/client"
	"github.com/ahplummer/gostafson/internal/config"
	"github.com/ahplummer/gostafson/internal/loop"
)

// script is a fake model that returns its replies in order.
type script []client.Message

func (s *script) Chat(_ context.Context, _ client.Request) (client.Reply, error) {
	m := (*s)[0]
	*s = (*s)[1:]
	return client.Reply{Message: m}, nil
}

func call(name, args string) client.Message {
	return client.Message{Role: "assistant", ToolCalls: []client.ToolCall{{ID: "1", Type: "function",
		Function: client.FunctionCall{Name: name, Arguments: args}}}}
}

func TestRunChecks(t *testing.T) {
	fixtures := t.TempDir()
	os.MkdirAll(filepath.Join(fixtures, "f"), 0o700)
	os.WriteFile(filepath.Join(fixtures, "f", "a.txt"), []byte("old"), 0o600)
	p := &config.Profile{Name: "code", Model: "m", Tools: []string{"read_file", "write_file"}}
	yes, no := true, false

	// Approved overwrite: passes every check.
	s := script{call("read_file", `{"path":"a.txt"}`), call("write_file", `{"path":"a.txt","content":"new"}`), {Role: "assistant", Content: "Updated a.txt."}}
	env := func(c loop.Chatter) Env {
		return Env{Profiles: map[string]*config.Profile{"code": p}, Clients: map[string]loop.Chatter{"code": c}, Fixtures: fixtures, MaxSteps: 8}
	}
	r := Run(context.Background(), env(&s), Case{Name: "ok", Profile: "code", Fixture: "f", Approve: "yes", WantAsk: &yes,
		WantTools: []string{"read_file", "write_file"}, ReplyAll: []string{"updated"},
		Files: []FileCheck{{Path: "a.txt", Contains: []string{"new"}}}})
	if !r.Pass || r.Steps != 3 || len(r.StepMS) != 3 || r.ToolErrors != 0 {
		t.Errorf("approved: %+v", r)
	}

	// Denied overwrite plus a path outside the root: the file is unchanged, the
	// denial isn't a tool error, the escape is.
	s = script{call("read_file", `{"path":"../x"}`), call("write_file", `{"path":"a.txt","content":"new"}`), {Role: "assistant", Content: "Done!"}}
	r = Run(context.Background(), env(&s), Case{Name: "no", Profile: "code", Fixture: "f", WantAsk: &no,
		ReplyAny: []string{"declin"}, Files: []FileCheck{{Path: "a.txt", Unchanged: true}, {Path: "b.txt", Absent: true}}})
	if r.Pass || r.ToolErrors != 1 || len(r.Failures) != 2 {
		t.Errorf("denied: pass %v, tool errors %d, failures %q", r.Pass, r.ToolErrors, r.Failures)
	}
}

// A multi-turn case routes each message like a real chat: the untagged
// follow-up stays on @code, and the history reaches the second turn.
func TestRunTurnsSticky(t *testing.T) {
	code := &config.Profile{Name: "code", Model: "m", BaseURL: "http://localhost/v1", Tools: []string{"write_file"}}
	def := &config.Profile{Name: "default", Model: "m", BaseURL: "http://localhost/v1"}
	var seen []int
	s := scriptFn(func(req client.Request) client.Message {
		seen = append(seen, len(req.Messages))
		if len(seen) == 2 {
			return call("write_file", `{"path":"package.json","content":"{}"}`)
		}
		return client.Message{Role: "assistant", Content: "ok"}
	})
	env := Env{Profiles: map[string]*config.Profile{"code": code, "default": def}, Default: "default",
		Clients: map[string]loop.Chatter{"code": s, "default": s}, Fixtures: t.TempDir(), MaxSteps: 8}
	r := Run(context.Background(), env, Case{Name: "t", Turns: []string{"@code shall I?", "yes"},
		WantProfiles: []string{"code", "code"}, Files: []FileCheck{{Path: "package.json", Contains: []string{"{}"}}}})
	if !r.Pass {
		t.Fatalf("result: %+v", r)
	}
	if seen[1] <= seen[0] {
		t.Errorf("second turn saw %d messages, first %d: history not carried", seen[1], seen[0])
	}
}

type scriptFn func(client.Request) client.Message

func (f scriptFn) Chat(_ context.Context, req client.Request) (client.Reply, error) {
	return client.Reply{Message: f(req)}, nil
}
