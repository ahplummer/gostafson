package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runRoot(t *testing.T) (*os.Root, string) {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app.js"), []byte("const a = 1;\nfunction connectRelay() {}\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "bad.js"), []byte("function (\n"), 0o600)
	os.MkdirAll(filepath.Join(dir, "sub"), 0o700)
	os.WriteFile(filepath.Join(dir, "sub", "notes.md"), []byte("TODO: dark mode\n"), 0o600)
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r, dir
}

func runJSON(cmd string, args ...string) json.RawMessage {
	b, _ := json.Marshal(runArgs{Command: cmd, Args: args})
	return b
}

func TestRunAllowed(t *testing.T) {
	r, _ := runRoot(t)
	for _, tc := range []struct {
		cmd  string
		args []string
		want string
	}{
		{"ls", []string{"-la"}, "app.js"},
		{"wc", []string{"-l", "app.js"}, "2 app.js"},
		{"grep", []string{"-rn", "TODO", "."}, "sub/notes.md:1:TODO"},
		{"head", []string{"-n", "1", "app.js"}, "const a"},
		{"find", []string{".", "-name", "*.md"}, "./sub/notes.md"},
		{"grep", []string{"--", "-x", "app.js"}, "[exit status 1]"}, // a pattern starting with -
	} {
		out, err := run(context.Background(), r, runJSON(tc.cmd, tc.args...))
		if err != nil || !strings.Contains(out, tc.want) {
			t.Errorf("%s %v = %q, %v; want %q", tc.cmd, tc.args, out, err, tc.want)
		}
	}
	if _, err := os.Stat("/opt/homebrew/bin/node"); err == nil {
		out, _ := run(context.Background(), r, runJSON("node", "--check", "bad.js"))
		if !strings.Contains(out, "SyntaxError") {
			t.Errorf("node --check bad.js = %q", out)
		}
	}
}

func TestRunRefused(t *testing.T) {
	r, dir := runRoot(t)
	os.Symlink("/etc", filepath.Join(dir, "etc"))
	os.MkdirAll(filepath.Join(dir, ".git"), 0o700)
	for _, tc := range []struct {
		cmd  string
		args []string
	}{
		{"sh", []string{"-c", "id"}},
		{"curl", []string{"https://example.com"}},
		{"bash", nil},
		{"node", []string{"app.js"}},           // running code
		{"node", []string{"-e", "1"}},          // running code
		{"python3", []string{"-c", "1"}},       // not on the list
		{"npm", []string{"install"}},           // not on the list
		{"grep", []string{"-R", "x", "."}},     // follows symlinks
		{"rg", []string{"--pre", "sh", "x"}},   // runs a program per file
		{"find", []string{".", "-exec", "id"}}, // runs a program
		{"find", []string{".", "-delete"}},
		{"jq", []string{"--rawfile", "a", "/etc/hosts", "."}},
		{"jq", []string{"-x", "app.js"}},
		{"cat", []string{"/etc/hosts"}},
		{"head", []string{"/etc/hosts"}},
		{"head", []string{"../x"}},
		{"head", []string{"~/secrets.toml"}},
		{"head", []string{"etc/hosts"}}, // symlink out of the root
		{"ls", []string{".git"}},
		{"rm", []string{".git/config"}},
		{"git status", []string{"--output=x"}},
		{"git diff", []string{"--ext-diff"}},
		{"git log", []string{"-c", "core.pager=id"}},
		{"git", []string{"config", "core.fsmonitor", "id"}},
		{"git commit", []string{"-a"}}, // -m is required
		{"ls", []string{"-l=x"}},
		{"mv", []string{"app.js"}}, // two paths needed
	} {
		raw := runJSON(tc.cmd, tc.args...)
		if err := checkRun(r, raw); err == nil {
			t.Errorf("%s %v: allowed, want refused", tc.cmd, tc.args)
		}
		if out, err := run(context.Background(), r, raw); err == nil {
			t.Errorf("%s %v: ran: %q", tc.cmd, tc.args, out)
		}
	}
}

func TestRunArgv(t *testing.T) {
	r, dir := runRoot(t)
	os.WriteFile(filepath.Join(dir, "-rf"), []byte("x"), 0o600)
	for _, tc := range []struct {
		cmd  string
		args []string
		want string
	}{
		{"rm", []string{"--", "-rf"}, "./-rf"},                       // a path, not a flag
		{"find", []string{"-name", "*.md", "sub"}, "sub -name *.md"}, // paths first for find
		{"git", []string{"diff", "HEAD~1", "app.js"}, "HEAD~1 -- app.js"},
		{"grep", []string{"x", "app.js", "-n"}, "-n x app.js"},
	} {
		c, err := prepare(r, runJSON(tc.cmd, tc.args...))
		if err != nil {
			t.Errorf("%s %v: %v", tc.cmd, tc.args, err)
			continue
		}
		if got := strings.Join(c.argv, " "); !strings.HasSuffix(got, tc.want) {
			t.Errorf("%s %v: argv %q, want suffix %q", tc.cmd, tc.args, got, tc.want)
		}
	}
}

func TestRunConfirm(t *testing.T) {
	r, _ := runRoot(t)
	if q := confirmRun(r, runJSON("rm", "app.js")); q != "run: rm app.js?" {
		t.Errorf("rm question = %q", q)
	}
	if q := confirmRun(r, runJSON("git commit", "-m", "first version")); !strings.Contains(q, `commit -m "first version"`) {
		t.Errorf("commit question = %q", q)
	}
	for _, c := range [][]string{{"ls"}, {"mkdir", "-p", "new/dir"}, {"git status"}} {
		if q := confirmRun(r, runJSON(c[0], c[1:]...)); q != "" {
			t.Errorf("%v asked %q", c, q)
		}
	}
}

func TestRunGit(t *testing.T) {
	if _, err := prepare(nil, runJSON("git status")); err != nil && strings.Contains(err.Error(), "not installed") {
		t.Skip("git not installed")
	}
	r, _ := runRoot(t)
	for _, c := range [][]string{{"git init"}, {"git add", "-A"}, {"git commit", "-m", "first"}, {"git status", "-s"}} {
		if out, err := run(context.Background(), r, runJSON(c[0], c[1:]...)); err != nil || strings.Contains(out, "exit status") {
			t.Fatalf("%v = %q, %v", c, out, err)
		}
	}
	out, err := run(context.Background(), r, runJSON("git log", "--oneline"))
	if err != nil || !strings.Contains(out, "first") {
		t.Errorf("git log = %q, %v", out, err)
	}
}

// The sandbox stops a command from reading a private directory, writing
// outside the root, or using the network, whatever its arguments.
func TestSandbox(t *testing.T) {
	if _, err := os.Stat(SandboxExec); err != nil {
		t.Skip("no sandbox-exec")
	}
	root, _ := filepath.EvalSymlinks(t.TempDir())
	private, _ := filepath.EvalSymlinks(t.TempDir())
	secret := filepath.Join(private, "secrets.toml")
	os.WriteFile(secret, []byte("token = 'x'"), 0o600)
	old := privateDirs
	privateDirs = []string{private}
	defer func() { privateDirs = old }()
	ctx := context.Background()

	if out, _ := Sandboxed(ctx, root, nil, "/bin/cat", secret); !strings.Contains(out, "Operation not permitted") {
		t.Errorf("read of private file: %q", out)
	}
	outside := filepath.Join(private, "new.txt")
	Sandboxed(ctx, root, nil, "/usr/bin/touch", outside)
	if _, err := os.Stat(outside); err == nil {
		t.Error("wrote outside the root")
	}
	if out, _ := Sandboxed(ctx, root, nil, "/usr/bin/touch", filepath.Join(root, "ok.txt")); strings.Contains(out, "exit status") {
		t.Errorf("write inside the root: %q", out)
	}
	if out, _ := Sandboxed(ctx, root, nil, "/usr/bin/nc", "-z", "-G", "2", "1.1.1.1", "443"); !strings.Contains(out, "exit status") {
		t.Errorf("network: %q", out)
	}
	if out, _ := Sandboxed(ctx, root, nil, "/bin/ls", root); strings.Contains(out, "exit status") {
		t.Errorf("ls of the root: %q", out)
	}
}

// On the host the root is inside a private directory (/Users/agent/work/napps
// under /Users). Commands work in the root but can't read its parents or
// anything else in the private directory.
func TestSandboxRootInsidePrivate(t *testing.T) {
	if _, err := os.Stat(SandboxExec); err != nil {
		t.Skip("no sandbox-exec")
	}
	private, _ := filepath.EvalSymlinks(t.TempDir())
	root := filepath.Join(private, "agent", "work")
	os.MkdirAll(root, 0o700)
	os.WriteFile(filepath.Join(private, "agent", "secrets.toml"), []byte("token = 'x'"), 0o600)
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o600)
	old := privateDirs
	privateDirs = []string{private}
	defer func() { privateDirs = old }()
	ctx := context.Background()

	r, _ := os.OpenRoot(root)
	defer r.Close()
	for _, c := range [][]string{{"ls", "-la"}, {"git init"}, {"git status"}} {
		if c[0] != "ls" {
			if _, err := prepare(r, runJSON(c[0])); err != nil {
				continue // git not installed
			}
		}
		if out, err := run(ctx, r, runJSON(c[0], c[1:]...)); err != nil || strings.Contains(out, "not permitted") {
			t.Errorf("%v in the root: %q, %v", c, out, err)
		}
	}
	for _, a := range [][]string{{"/bin/cat", filepath.Join(private, "agent", "secrets.toml")}, {"/bin/ls", filepath.Join(private, "agent")}} {
		if out, _ := Sandboxed(ctx, root, nil, a[0], a[1:]...); !strings.Contains(out, "not permitted") {
			t.Errorf("%v: %q", a, out)
		}
	}
}

func TestRunCommandString(t *testing.T) {
	r, _ := runRoot(t)
	for _, tc := range []struct{ cmd, want string }{
		{"git add .", "-- ."},
		{`git commit -m "first version"`, "-m first version"},
		{`git commit -m "fix | and ;"`, "-m fix | and ;"}, // quoted: just text
		{"wc -l app.js", "-l app.js"},
	} {
		c, err := prepare(r, runJSON(tc.cmd))
		if err != nil {
			t.Errorf("%q: %v", tc.cmd, err)
			continue
		}
		if got := strings.Join(c.argv, " "); !strings.HasSuffix(got, tc.want) {
			t.Errorf("%q: argv %q, want suffix %q", tc.cmd, got, tc.want)
		}
	}
	for _, bad := range []string{"sh -c id", "wc -l app.js | sh", `git commit -m "open`, "ls; id", ""} {
		if err := checkRun(r, runJSON(bad)); err == nil {
			t.Errorf("%q: allowed", bad)
		}
	}
}
