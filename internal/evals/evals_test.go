package evals

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ahplummer/gostafson/internal/client"
	"github.com/ahplummer/gostafson/internal/config"
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
	r := Run(context.Background(), &s, p, Case{Name: "ok", Fixture: "f", Approve: "yes", WantAsk: &yes,
		WantTools: []string{"read_file", "write_file"}, ReplyAll: []string{"updated"},
		Files: []FileCheck{{Path: "a.txt", Contains: []string{"new"}}}}, fixtures, 8)
	if !r.Pass || r.Steps != 3 || len(r.StepMS) != 3 || r.ToolErrors != 0 {
		t.Errorf("approved: %+v", r)
	}

	// Denied overwrite plus a path outside the root: the file is unchanged, the
	// denial isn't a tool error, the escape is.
	s = script{call("read_file", `{"path":"../x"}`), call("write_file", `{"path":"a.txt","content":"new"}`), {Role: "assistant", Content: "Done!"}}
	r = Run(context.Background(), &s, p, Case{Name: "no", Fixture: "f", WantAsk: &no,
		ReplyAny: []string{"declin"}, Files: []FileCheck{{Path: "a.txt", Unchanged: true}, {Path: "b.txt", Absent: true}}}, fixtures, 8)
	if r.Pass || r.ToolErrors != 1 || len(r.Failures) != 2 {
		t.Errorf("denied: pass %v, tool errors %d, failures %q", r.Pass, r.ToolErrors, r.Failures)
	}
}
