// Package tools implements the file tools. Every tool works on an *os.Root,
// so a path from the model can't leave the root through "..", an absolute
// path or a symlink: os.Root rejects those itself. The guard package decides
// which tools a profile gets and logs each call.
package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/ahplummer/gostafson/internal/client"
)

// Limits that keep tool results small enough for the model's context.
const (
	MaxOutput     = 16 << 10 // bytes returned by any tool
	MaxWrite      = 1 << 20  // bytes accepted by write_file
	maxSearchFile = 1 << 20  // larger files are skipped by search_files
	maxMatches    = 50
	maxEntries    = 500
	maxLine       = 200
)

// Tool is one callable tool.
type Tool struct {
	Def client.Tool
	Run func(ctx context.Context, root *os.Root, args json.RawMessage) (string, error)
	// Confirm, if set, returns a question to ask the owner before Run, or ""
	// when this call needs no approval.
	Confirm func(root *os.Root, args json.RawMessage) string
}

// All returns every tool by name.
func All() map[string]Tool {
	return map[string]Tool{
		"list_dir":     {Def: def("list_dir", "List the entries of a directory. Directories end with /.", pathSchema), Run: listDir},
		"read_file":    {Def: def("read_file", "Read a text file.", pathSchema), Run: readFile},
		"write_file":   {Def: def("write_file", "Create or overwrite a text file. Parent directories are created.", writeSchema), Run: writeFile, Confirm: confirmOverwrite},
		"search_files": {Def: def("search_files", "Find lines containing a string (case-insensitive) in text files under a directory.", searchSchema), Run: searchFiles},
	}
}

// Names returns the tool names, sorted.
func Names() []string {
	var n []string
	for k := range All() {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

const (
	pathSchema = `{"type":"object","properties":{"path":{"type":"string","description":"Path relative to the working directory; \".\" is the top."}},"required":["path"]}`

	writeSchema = `{"type":"object","properties":{
		"path":{"type":"string","description":"Path relative to the working directory."},
		"content":{"type":"string","description":"The complete new file contents."}},"required":["path","content"]}`

	searchSchema = `{"type":"object","properties":{
		"query":{"type":"string","description":"Text to find."},
		"path":{"type":"string","description":"Directory to search, relative to the working directory. Default \".\"."}},"required":["query"]}`
)

func def(name, desc, schema string) client.Tool {
	return client.Tool{Type: "function", Function: client.ToolFunction{Name: name, Description: desc, Parameters: json.RawMessage(schema)}}
}

// Rel turns a model-supplied path into a clean relative path. It rejects
// absolute paths and paths that climb out with "..". This is a readable first
// check that the guard logs; os.Root is what actually enforces the jail
// (including symlinks), so a bug here can't open anything outside the root.
func Rel(p string) (string, error) {
	if p == "" {
		return ".", nil
	}
	if filepath.IsAbs(p) || strings.HasPrefix(p, "~") {
		return "", fmt.Errorf("absolute path %q: use a path relative to the working directory", p)
	}
	c := filepath.Clean(p)
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("path %q is outside the working directory", p)
	}
	return c, nil
}

func decode(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("bad arguments: %w", err)
	}
	return nil
}

func listDir(_ context.Context, root *os.Root, raw json.RawMessage) (string, error) {
	var a struct{ Path string }
	if err := decode(raw, &a); err != nil {
		return "", err
	}
	p, err := Rel(a.Path)
	if err != nil {
		return "", err
	}
	f, err := root.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	var b strings.Builder
	for i, e := range entries {
		if i == maxEntries {
			fmt.Fprintf(&b, "... %d more entries\n", len(entries)-maxEntries)
			break
		}
		switch {
		case e.IsDir():
			fmt.Fprintf(&b, "%s/\n", e.Name())
		case e.Type()&fs.ModeSymlink != 0:
			fmt.Fprintf(&b, "%s@\n", e.Name())
		default:
			size := int64(0)
			if info, err := e.Info(); err == nil {
				size = info.Size()
			}
			fmt.Fprintf(&b, "%s  %d bytes\n", e.Name(), size)
		}
	}
	if b.Len() == 0 {
		return "(empty directory)", nil
	}
	return b.String(), nil
}

func readFile(_ context.Context, root *os.Root, raw json.RawMessage) (string, error) {
	var a struct{ Path string }
	if err := decode(raw, &a); err != nil {
		return "", err
	}
	p, err := Rel(a.Path)
	if err != nil {
		return "", err
	}
	f, err := root.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory; use list_dir", a.Path)
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxOutput+1))
	if err != nil {
		return "", err
	}
	if !isText(data) {
		return "", fmt.Errorf("%s is not a text file", a.Path)
	}
	if len(data) > MaxOutput {
		data = data[:MaxOutput]
		// Drop a rune cut in half by the limit.
		for len(data) > 0 {
			if r, size := utf8.DecodeLastRune(data); r != utf8.RuneError || size > 1 {
				break
			}
			data = data[:len(data)-1]
		}
		return fmt.Sprintf("%s\n[truncated: first %d of %d bytes]", data, len(data), info.Size()), nil
	}
	return string(data), nil
}

func writeFile(_ context.Context, root *os.Root, raw json.RawMessage) (string, error) {
	var a struct{ Path, Content string }
	if err := decode(raw, &a); err != nil {
		return "", err
	}
	p, err := Rel(a.Path)
	if err != nil {
		return "", err
	}
	if p == "." {
		return "", errors.New("path is required")
	}
	if len(a.Content) > MaxWrite {
		return "", fmt.Errorf("content is %d bytes; limit is %d", len(a.Content), MaxWrite)
	}
	if dir := filepath.Dir(p); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if err := root.WriteFile(p, []byte(a.Content), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(a.Content), p), nil
}

// confirmOverwrite asks before write_file replaces an existing file, showing
// the size change: a large drop is the usual sign that a model truncated the
// file. New files need no approval.
func confirmOverwrite(root *os.Root, raw json.RawMessage) string {
	var a struct{ Path, Content string }
	if decode(raw, &a) != nil {
		return ""
	}
	p, err := Rel(a.Path)
	if err != nil {
		return ""
	}
	info, err := root.Stat(p)
	if err != nil || !info.Mode().IsRegular() {
		return "" // new file, or an error writeFile will report
	}
	return fmt.Sprintf("overwrite %s (%s -> %s bytes)?", p, commas(info.Size()), commas(int64(len(a.Content))))
}

func commas(n int64) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func searchFiles(ctx context.Context, root *os.Root, raw json.RawMessage) (string, error) {
	var a struct{ Query, Path string }
	if err := decode(raw, &a); err != nil {
		return "", err
	}
	if a.Query == "" {
		return "", errors.New("query is required")
	}
	p, err := Rel(a.Path)
	if err != nil {
		return "", err
	}
	query := strings.ToLower(a.Query)
	fsys := root.FS() // fs.WalkDir doesn't follow symlinks, and root.FS can't leave the root anyway

	var b strings.Builder
	matches := 0
	err = fs.WalkDir(fsys, filepath.ToSlash(p), func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if name != "." && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules") {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if info, err := d.Info(); err != nil || info.Size() > maxSearchFile {
			return nil
		}
		data, err := fs.ReadFile(fsys, name)
		if err != nil || !isText(data) {
			return nil
		}
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 64<<10), maxSearchFile)
		for line := 1; sc.Scan(); line++ {
			text := sc.Text()
			if !strings.Contains(strings.ToLower(text), query) {
				continue
			}
			if matches == maxMatches {
				b.WriteString("[stopped at 50 matches]\n")
				return fs.SkipAll
			}
			matches++
			text = strings.TrimSpace(text)
			if len(text) > maxLine {
				text = text[:maxLine] + "..."
			}
			fmt.Fprintf(&b, "%s:%d: %s\n", path.Clean(name), line, text)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if matches == 0 {
		return "no matches", nil
	}
	out := b.String()
	if len(out) > MaxOutput {
		out = out[:MaxOutput] + "\n[truncated]"
	}
	return out, nil
}

// isText reports whether data looks like UTF-8 text without NUL bytes.
// A cut multibyte rune at the end (from a size limit) is allowed.
func isText(data []byte) bool {
	if bytes.IndexByte(data, 0) >= 0 {
		return false
	}
	for i := 0; i < utf8.UTFMax && len(data) > 0; i++ {
		if utf8.Valid(data) {
			return true
		}
		data = data[:len(data)-1]
	}
	return utf8.Valid(data)
}
