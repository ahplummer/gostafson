package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setup(t *testing.T) *os.Root {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\nworld\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "sub", "b.md"), []byte("# Title\nHello again\n"), 0o644)
	os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("hello from git\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "bin"), []byte{0x7f, 'E', 'L', 'F', 0, 1}, 0o644)
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func call(t *testing.T, r *os.Root, name, args string) (string, error) {
	t.Helper()
	return All()[name].Run(context.Background(), r, json.RawMessage(args))
}

func TestListDir(t *testing.T) {
	r := setup(t)
	out, err := call(t, r, "list_dir", `{"path":"."}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"a.txt  12 bytes", "sub/", ".git/"} {
		if !strings.Contains(out, want) {
			t.Errorf("list_dir missing %q:\n%s", want, out)
		}
	}
}

func TestReadFile(t *testing.T) {
	r := setup(t)
	if out, err := call(t, r, "read_file", `{"path":"./sub/b.md"}`); err != nil || !strings.HasPrefix(out, "# Title") {
		t.Errorf("read sub/b.md = %q, %v", out, err)
	}
	if _, err := call(t, r, "read_file", `{"path":"bin"}`); err == nil || !strings.Contains(err.Error(), "not a text file") {
		t.Errorf("binary read err = %v", err)
	}
	if _, err := call(t, r, "read_file", `{"path":"sub"}`); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Errorf("dir read err = %v", err)
	}
}

func TestReadFileTruncates(t *testing.T) {
	r := setup(t)
	r.WriteFile("big.txt", []byte(strings.Repeat("é", MaxOutput)), 0o644) // 2 bytes each
	out, err := call(t, r, "read_file", `{"path":"big.txt"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[truncated: first 16384 of 32768 bytes]") {
		t.Errorf("tail = %q", out[len(out)-60:])
	}
}

func TestWriteFile(t *testing.T) {
	r := setup(t)
	if _, err := call(t, r, "write_file", `{"path":"new/dir/c.html","content":"<p>hi</p>"}`); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.ReadFile("new/dir/c.html"); string(got) != "<p>hi</p>" {
		t.Errorf("written = %q", got)
	}
	if _, err := call(t, r, "write_file", `{"path":"","content":"x"}`); err == nil {
		t.Error("empty path: expected error")
	}
}

func TestSearchFiles(t *testing.T) {
	r := setup(t)
	out, err := call(t, r, "search_files", `{"query":"HELLO"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.txt:1: hello") || !strings.Contains(out, "sub/b.md:2: Hello again") {
		t.Errorf("search = %q", out)
	}
	if strings.Contains(out, ".git") {
		t.Errorf("search should skip dot directories: %q", out)
	}
	if out, _ := call(t, r, "search_files", `{"query":"nothing here"}`); out != "no matches" {
		t.Errorf("no-match search = %q", out)
	}
}

func TestRel(t *testing.T) {
	for in, want := range map[string]string{"": ".", ".": ".", "./a": "a", "a/../b": "b", "a/": "a"} {
		if got, err := Rel(in); err != nil || got != want {
			t.Errorf("Rel(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"..", "../x", "a/../../x", "/etc/passwd", "~/x"} {
		if _, err := Rel(in); err == nil {
			t.Errorf("Rel(%q): expected error", in)
		}
	}
}
