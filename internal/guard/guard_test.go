package guard

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ahplummer/gostafson/internal/approve"
	"github.com/ahplummer/gostafson/internal/audit"
)

const secret = "TOP-SECRET-OUTSIDE"

// fixture builds:
//
//	base/outside/secret.txt         must never be read or listed
//	base/outside/dir/               must never be written to
//	base/root/a.txt
//	base/root/link-file -> ../outside/secret.txt
//	base/root/link-dir  -> ../outside/dir
//	base/root/link-abs  -> <absolute path of outside/secret.txt>
//	base/root/inside    -> a.txt    (symlink within the root: allowed)
func fixture(t *testing.T, toolNames ...string) (*Guard, string, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	must(t, os.MkdirAll(filepath.Join(outside, "dir"), 0o755))
	must(t, os.MkdirAll(root, 0o755))
	must(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte(secret), 0o644))
	must(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("inside file"), 0o644))
	must(t, os.Symlink("../outside/secret.txt", filepath.Join(root, "link-file")))
	must(t, os.Symlink("../outside/dir", filepath.Join(root, "link-dir")))
	must(t, os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "link-abs")))
	must(t, os.Symlink("a.txt", filepath.Join(root, "inside")))

	logPath := filepath.Join(base, "audit.jsonl")
	log, err := audit.Open(logPath)
	must(t, err)
	t.Cleanup(func() { log.Close() })
	if len(toolNames) == 0 {
		toolNames = []string{"list_dir", "read_file", "write_file", "search_files"}
	}
	g, err := New("code", toolNames, root, log)
	must(t, err)
	t.Cleanup(func() { g.Close() })
	return g, outside, logPath
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// TestEscapeAttempts is the jail test. Every case must fail without leaking
// outside content or writing outside the root. A passing escape fails make test,
// which blocks deploy.
func TestEscapeAttempts(t *testing.T) {
	g, outside, _ := fixture(t)
	turn := g.ForTurn("c", audit.NewTraceID(), audit.NewSpanID(), nil)
	abs := filepath.Join(outside, "secret.txt")

	cases := []struct{ name, tool, args string }{
		{"dotdot", "read_file", `{"path":"../outside/secret.txt"}`},
		{"dotdot nested", "read_file", `{"path":"a/../../outside/secret.txt"}`},
		{"absolute", "read_file", `{"path":"` + abs + `"}`},
		{"home", "read_file", `{"path":"~/.ssh/id_ed25519"}`},
		{"symlink file", "read_file", `{"path":"link-file"}`},
		{"symlink absolute", "read_file", `{"path":"link-abs"}`},
		{"symlink dir read", "read_file", `{"path":"link-dir/x.txt"}`},
		{"list parent", "list_dir", `{"path":".."}`},
		{"list absolute", "list_dir", `{"path":"/"}`},
		{"list symlink dir", "list_dir", `{"path":"link-dir"}`},
		{"write dotdot", "write_file", `{"path":"../outside/dir/evil.txt","content":"x"}`},
		{"write absolute", "write_file", `{"path":"` + filepath.Join(outside, "dir", "evil.txt") + `","content":"x"}`},
		{"write through symlink dir", "write_file", `{"path":"link-dir/evil.txt","content":"x"}`},
		{"write over symlink file", "write_file", `{"path":"link-file","content":"x"}`},
		{"search parent", "search_files", `{"query":"secret","path":".."}`},
		{"search symlink dir", "search_files", `{"query":"x","path":"link-dir"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := turn.Call(context.Background(), c.tool, c.args)
			if err == nil {
				t.Errorf("%s %s: expected error, got %q", c.tool, c.args, out)
			}
			if strings.Contains(out, secret) || (err != nil && strings.Contains(err.Error(), secret)) {
				t.Errorf("leaked outside content: %q %v", out, err)
			}
		})
	}

	if got, _ := os.ReadFile(abs); string(got) != secret {
		t.Errorf("outside file changed: %q", got)
	}
	if entries, _ := os.ReadDir(filepath.Join(outside, "dir")); len(entries) != 0 {
		t.Errorf("files written outside the root: %v", entries)
	}
}

// Search from the top of the root must not walk into symlinked directories.
func TestSearchDoesNotFollowSymlinks(t *testing.T) {
	g, _, _ := fixture(t)
	out, err := g.ForTurn("c", "t", "s", nil).Call(context.Background(), "search_files", `{"query":"secret"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, secret) {
		t.Errorf("search followed a symlink out of the root: %q", out)
	}
}

func TestAllowedInsideRoot(t *testing.T) {
	g, _, _ := fixture(t)
	turn := g.ForTurn("c", "t", "s", nil)
	ctx := context.Background()
	if out, err := turn.Call(ctx, "read_file", `{"path":"inside"}`); err != nil || out != "inside file" {
		t.Errorf("symlink within root = %q, %v", out, err)
	}
	if _, err := turn.Call(ctx, "write_file", `{"path":"web/index.html","content":"<h1>ok</h1>"}`); err != nil {
		t.Errorf("write inside root: %v", err)
	}
	if out, err := turn.Call(ctx, "read_file", `{"path":"web/index.html"}`); err != nil || out != "<h1>ok</h1>" {
		t.Errorf("read back = %q, %v", out, err)
	}
}

func TestDefaultDeny(t *testing.T) {
	g, _, _ := fixture(t, "read_file")
	turn := g.ForTurn("c", "t", "s", nil)
	ctx := context.Background()

	if defs := turn.Definitions(); len(defs) != 1 || defs[0].Function.Name != "read_file" {
		t.Errorf("definitions = %+v", defs)
	}
	if _, err := turn.Call(ctx, "write_file", `{"path":"a.txt","content":"x"}`); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("unlisted tool err = %v", err)
	}
	if _, err := turn.Call(ctx, "run_shell", `{"cmd":"cat /etc/passwd"}`); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("unknown tool err = %v", err)
	}
	if _, err := turn.Call(ctx, "read_file", `not json`); err == nil {
		t.Error("bad arguments: expected error")
	}
	if b, _ := os.ReadFile(filepath.Join(g.Root(), "a.txt")); string(b) != "inside file" {
		t.Errorf("denied write changed the file: %q", b)
	}
}

func TestNoToolsProfile(t *testing.T) {
	g, err := New("default", nil, "", nil)
	must(t, err)
	turn := g.ForTurn("c", "t", "s", nil)
	if len(turn.Definitions()) != 0 {
		t.Error("expected no definitions")
	}
	if _, err := turn.Call(context.Background(), "read_file", `{"path":"a.txt"}`); err == nil {
		t.Error("tool call on a no-tools profile: expected error")
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	if _, err := New("code", []string{"rm_rf"}, t.TempDir(), nil); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("unknown tool in config: %v", err)
	}
	if _, err := New("code", []string{"read_file"}, "", nil); err == nil {
		t.Error("tools without root: expected error")
	}
}

func TestAuditRecords(t *testing.T) {
	g, _, logPath := fixture(t, "read_file")
	turn := g.ForTurn("chat1", "trace1", "span1", nil)
	ctx := context.Background()
	turn.Call(ctx, "read_file", `{"path":"a.txt"}`)
	turn.Call(ctx, "read_file", `{"path":"../outside/secret.txt"}`)
	turn.Call(ctx, "read_file", `{"path":"link-file"}`)
	turn.Call(ctx, "write_file", `{"path":"a.txt","content":"x"}`)

	f, err := os.Open(logPath)
	must(t, err)
	defer f.Close()
	var got []audit.ToolCall
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r audit.ToolCall
		must(t, json.Unmarshal(sc.Bytes(), &r))
		got = append(got, r)
	}
	if len(got) != 4 {
		t.Fatalf("%d records, want 4", len(got))
	}
	want := []struct {
		guard, reason string
		hasErr        bool
	}{
		{"allow", "", false},
		{"deny", "path outside root", true},
		{"allow", "", true}, // passes the path check; os.Root rejects the symlink
		{"deny", "tool not allowed for this profile", true},
	}
	for i, w := range want {
		r := got[i]
		if r.Guard != w.guard || r.Reason != w.reason || (r.Error != "") != w.hasErr {
			t.Errorf("record %d = guard %q reason %q error %q", i, r.Guard, r.Reason, r.Error)
		}
		if r.TraceID != "trace1" || r.ParentSpanID != "span1" || r.Chat != "chat1" || r.Profile != "code" || len(r.SpanID) != 16 {
			t.Errorf("record %d ids = %+v", i, r)
		}
	}
	if !strings.Contains(string(got[0].Args), `"path":"a.txt"`) {
		t.Errorf("args = %s", got[0].Args)
	}
}

type fakeApprover struct {
	result approve.Result
	asked  []string
}

func (f *fakeApprover) Approve(_ context.Context, _, q string) approve.Result {
	f.asked = append(f.asked, q)
	return f.result
}

func TestOverwriteNeedsApproval(t *testing.T) {
	ctx := context.Background()
	write := `{"path":"a.txt","content":"replaced"}`
	read := func(g *Guard) string {
		b, _ := os.ReadFile(filepath.Join(g.Root(), "a.txt"))
		return string(b)
	}

	for _, result := range []approve.Result{approve.Denied, approve.Timeout} {
		g, _, _ := fixture(t)
		ap := &fakeApprover{result: result}
		if _, err := g.ForTurn("c", "t", "s", ap).Call(ctx, "write_file", write); err == nil {
			t.Errorf("%s: expected error", result)
		}
		if len(ap.asked) != 1 || ap.asked[0] != "overwrite a.txt (11 -> 8 bytes)?" {
			t.Errorf("asked = %q", ap.asked)
		}
		if got := read(g); got != "inside file" {
			t.Errorf("%s: file changed to %q", result, got)
		}
	}

	// No approver (a transport that can't ask) is a no.
	g, _, _ := fixture(t)
	if _, err := g.ForTurn("c", "t", "s", nil).Call(ctx, "write_file", write); err == nil || read(g) != "inside file" {
		t.Errorf("nil approver: err %v, file %q", err, read(g))
	}

	ap := &fakeApprover{result: approve.Approved}
	if _, err := g.ForTurn("c", "t", "s", ap).Call(ctx, "write_file", write); err != nil || read(g) != "replaced" {
		t.Errorf("approved: err %v, file %q", err, read(g))
	}
}

func TestNewFileNeedsNoApproval(t *testing.T) {
	g, _, _ := fixture(t)
	ap := &fakeApprover{result: approve.Denied}
	if _, err := g.ForTurn("c", "t", "s", ap).Call(context.Background(), "write_file", `{"path":"new.txt","content":"x"}`); err != nil {
		t.Errorf("new file: %v", err)
	}
	if len(ap.asked) != 0 {
		t.Errorf("asked for a new file: %q", ap.asked)
	}
}

func TestApprovalInAudit(t *testing.T) {
	g, _, logPath := fixture(t)
	g.ForTurn("c", "t", "s", &fakeApprover{result: approve.Timeout}).Call(context.Background(), "write_file", `{"path":"a.txt","content":"x"}`)
	b, _ := os.ReadFile(logPath)
	var r audit.ToolCall
	must(t, json.Unmarshal(b, &r))
	if r.Guard != "deny" || r.Reason != "not approved" || r.Approval != "timeout" {
		t.Errorf("record = guard %q reason %q approval %q", r.Guard, r.Reason, r.Approval)
	}
}

// run through the guard: a refused command is logged as a denial and never
// starts; a read-only command runs without asking; one that changes files
// asks first and doesn't run on a no; a symlink can't take a command outside.
func TestRunThroughGuard(t *testing.T) {
	g, outside, logPath := fixture(t, "run")
	ap := &fakeApprover{result: approve.Denied}
	turn := g.ForTurn("c", "t", "s", ap)
	ctx := context.Background()

	if _, err := turn.Call(ctx, "run", `{"command":"sh","args":["-c","id"]}`); err == nil {
		t.Error("sh: allowed")
	}
	if out, err := turn.Call(ctx, "run", `{"command":"head","args":["link-file"]}`); err == nil || strings.Contains(out, secret) {
		t.Errorf("head link-file = %q, %v", out, err)
	}
	if out, err := turn.Call(ctx, "run", `{"command":"wc","args":["-c","a.txt"]}`); err != nil || !strings.Contains(out, "11 a.txt") {
		t.Errorf("wc = %q, %v", out, err)
	}
	if _, err := turn.Call(ctx, "run", `{"command":"rm","args":["a.txt"]}`); err == nil {
		t.Error("rm without approval: ran")
	}
	if len(ap.asked) != 1 || ap.asked[0] != "run: rm a.txt?" {
		t.Errorf("asked %q", ap.asked)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(outside), "root", "a.txt")); err != nil {
		t.Error("a.txt removed despite the no")
	}

	b, _ := os.ReadFile(logPath)
	recs := strings.Split(strings.TrimSpace(string(b)), "\n")
	var reasons []string
	for _, l := range recs {
		var r audit.ToolCall
		must(t, json.Unmarshal([]byte(l), &r))
		reasons = append(reasons, r.Guard+":"+r.Reason)
	}
	want := "deny:arguments not allowed,deny:arguments not allowed,allow:,deny:not approved"
	if got := strings.Join(reasons, ","); got != want {
		t.Errorf("audit = %s, want %s", got, want)
	}
}
