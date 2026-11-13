package lineedit

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// run types keys (each string one read, like a terminal delivers them) and
// returns the submitted lines.
func run(t *testing.T, e *Editor, opt Options, reads ...string) ([]Line, error) {
	t.Helper()
	in := make(chan []byte, len(reads)+1)
	for _, r := range reads {
		in <- []byte(r)
	}
	close(in)
	e.In, e.Out = in, io.Discard
	var lines []Line
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		l, err := e.ReadLine(ctx, "❯ ", opt)
		cancel()
		if err != nil {
			return lines, err
		}
		lines = append(lines, l)
	}
}

func texts(ls []Line) []string {
	var out []string
	for _, l := range ls {
		out = append(out, l.Text)
	}
	return out
}

func TestEditing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reads []string
		want  string
	}{
		{"typing", []string{"hello\r"}, "hello"},
		{"backspace", []string{"helxx\x7f\x7flo\r"}, "hello"},
		{"left and insert", []string{"hllo\x1b[D\x1b[D\x1b[De\r"}, "hello"},
		{"home and end", []string{"ello\x01h\x05!\r"}, "hello!"},
		{"kill word", []string{"hello big world\x17\x17there\r"}, "hello there"},
		{"kill to start", []string{"junk\x15fine\r"}, "fine"},
		{"kill to end", []string{"keep this\x01\x1b[C\x1b[C\x1b[C\x1b[C\x0b\r"}, "keep"},
		{"delete key", []string{"xhi\x01\x1b[3~\r"}, "hi"},
		{"utf-8", []string{"caf\xc3", "\xa9 ☕\r"}, "café ☕"},
		{"split escape", []string{"ab\x1b", "[D", "X\r"}, "aXb"},
		{"word left", []string{"one two\x1bbX\r"}, "one Xtwo"},
		{"ctrl-c clears", []string{"oops\x03ok\r"}, "ok"},
		{"multi-line paste", []string{"\x1b[200~line one\r\nline two\x1b[201~\r"}, "line one\nline two"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines, err := run(t, &Editor{}, Options{}, tc.reads...)
			if !errors.Is(err, io.EOF) || len(lines) != 1 || lines[0].Text != tc.want {
				t.Errorf("got %q, %v; want %q", texts(lines), err, tc.want)
			}
		})
	}
}

func TestEOFAndInterrupt(t *testing.T) {
	if _, err := run(t, &Editor{}, Options{}, "\x04"); !errors.Is(err, io.EOF) {
		t.Errorf("Ctrl-D on empty line: %v", err)
	}
	if _, err := run(t, &Editor{}, Options{}, "\x03", "\x03"); !errors.Is(err, ErrInterrupt) {
		t.Errorf("Ctrl-C twice: %v", err)
	}
}

func TestHistory(t *testing.T) {
	e := &Editor{}
	lines, _ := run(t, e, Options{History: true}, "first\r", "second\r", "\x1b[A\x1b[A\r", "dra\x1b[A\x1b[B\r")
	if got := strings.Join(texts(lines), "|"); got != "first|second|first|dra" {
		t.Errorf("lines = %s", got)
	}
}

func TestCompletion(t *testing.T) {
	cands := func(w string) []string {
		return []string{"/help", "/profiles", "/reset", "/status", "@code", "@cloud", "@default"}
	}
	for _, tc := range []struct{ reads, want string }{
		{"/he\t\r", "/help "},
		{"/pro\x1b[C\r", "/profiles"}, // right arrow accepts the hint
		{"@cod\tfix it\r", "@code fix it"},
		{"@c\t\r", "@c"}, // @code and @cloud: the common prefix is already typed
		{"@cl\t\r", "@cloud "},
		{"hello /he\t\r", "hello /he"}, // only at the start of the line
	} {
		lines, _ := run(t, &Editor{Complete: cands}, Options{Complete: true}, tc.reads)
		if len(lines) != 1 || lines[0].Text != tc.want {
			t.Errorf("%q -> %q, want %q", tc.reads, texts(lines), tc.want)
		}
	}
}

func TestClipboardImage(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nfake")
	e := &Editor{Clipboard: func(context.Context) ([]byte, string, error) { return png, "", nil }}
	lines, _ := run(t, e, Options{}, "what is \x16?\r")
	if len(lines) != 1 || lines[0].Text != "what is [image #1]?" || len(lines[0].Images) != 1 || string(lines[0].Images[0].Data) != string(png) {
		t.Fatalf("lines = %+v", lines)
	}
	// Deleting the marker drops the image.
	lines, _ = run(t, e, Options{}, "\x16\x17\x17\x17x\r")
	if len(lines) != 1 || len(lines[0].Images) != 0 {
		t.Errorf("marker deleted, images = %d (%q)", len(lines[0].Images), lines[0].Text)
	}
	// Text on the clipboard is pasted.
	e.Clipboard = func(context.Context) ([]byte, string, error) { return nil, "pasted text", nil }
	lines, _ = run(t, e, Options{}, ">\x16\r")
	if len(lines) != 1 || lines[0].Text != ">pasted text" {
		t.Errorf("text paste = %q", texts(lines))
	}
}

func TestImagePaths(t *testing.T) {
	dir := t.TempDir()
	shot := filepath.Join(dir, "Screen Shot 1.png")
	os.WriteFile(shot, []byte("\x89PNG..."), 0o600)
	escaped := strings.ReplaceAll(shot, " ", `\ `)
	for _, in := range []string{"look at " + escaped + " please", "look at '" + shot + "' please"} {
		text, ims := ImagePaths(in, 0)
		if text != "look at [image #1] please" || len(ims) != 1 || ims[0].Name != "Screen Shot 1.png" {
			t.Errorf("%q -> %q, %d images", in, text, len(ims))
		}
	}
	if text, ims := ImagePaths("see /no/such/file.png and notes.txt", 2); len(ims) != 0 || text != "see /no/such/file.png and notes.txt" {
		t.Errorf("missing file: %q, %d", text, len(ims))
	}
}

func TestParsePNGf(t *testing.T) {
	b, ok := parsePNGf("«data PNGf89504E470D0A1A0A0000»\n")
	if !ok || len(b) != 10 || string(b[1:4]) != "PNG" {
		t.Errorf("parse = %x %v", b, ok)
	}
	if _, ok := parsePNGf("missing value"); ok {
		t.Error("parsed garbage")
	}
}
