package transport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCLI(t *testing.T) {
	var out bytes.Buffer
	c := NewCLI(strings.NewReader("hello\n\n@code fix it\n"), &out)
	ctx := context.Background()

	m, err := c.Receive(ctx)
	if err != nil || m.Text != "hello" || m.ChatID != "cli" {
		t.Fatalf("first = %+v, %v", m, err)
	}
	m, err = c.Receive(ctx) // blank line skipped
	if err != nil || m.Text != "@code fix it" {
		t.Fatalf("second = %+v, %v", m, err)
	}
	if _, err := c.Receive(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("third err = %v, want EOF", err)
	}

	c.Send(ctx, "cli", "reply")
	if !strings.Contains(out.String(), "reply\n\n") {
		t.Errorf("out = %q", out.String())
	}
}

func TestCLIExit(t *testing.T) {
	for _, cmd := range []string{"/exit", "/quit", "  /exit  "} {
		c := NewCLI(strings.NewReader("hi\n"+cmd+"\nnever read\n"), io.Discard)
		if m, err := c.Receive(context.Background()); err != nil || m.Text != "hi" {
			t.Fatalf("%q: first = %+v, %v", cmd, m, err)
		}
		if _, err := c.Receive(context.Background()); !errors.Is(err, io.EOF) {
			t.Errorf("%q: err = %v, want EOF", cmd, err)
		}
	}
}

func TestCLIContextCancel(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()
	c := NewCLI(r, io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Receive(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}

func TestCLIAsk(t *testing.T) {
	var out bytes.Buffer
	c := NewCLI(strings.NewReader("@code write it\ny\nnext\n"), &out)
	ctx := context.Background()
	if m, _ := c.Receive(ctx); m.Text != "@code write it" {
		t.Fatalf("first = %q", m.Text)
	}
	if ans, err := c.Ask(ctx, "cli", "overwrite a.txt? [y/N]"); err != nil || ans != "y" {
		t.Errorf("ask = %q, %v", ans, err)
	}
	if !strings.Contains(out.String(), "overwrite a.txt? [y/N] ") {
		t.Errorf("out = %q", out.String())
	}
	if m, _ := c.Receive(ctx); m.Text != "next" {
		t.Errorf("after ask = %q", m.Text)
	}
}

func TestCLIAskTimeout(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()
	var out bytes.Buffer
	c := NewCLI(r, &out)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Ask(ctx, "cli", "q?"); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
	if !strings.Contains(out.String(), "treated as no") {
		t.Errorf("out = %q", out.String())
	}
}

func TestPlainCLIHasNoEscapes(t *testing.T) {
	var out bytes.Buffer
	c := NewCLI(strings.NewReader(""), &out)
	c.Banner("v1")
	c.Busy("cli")()
	c.Send(context.Background(), "cli", "hi `x`\n[code | m | 1.0s | 3 tok]")
	if strings.Contains(out.String(), "\033") {
		t.Errorf("escape codes without color: %q", out.String())
	}
	if !strings.Contains(out.String(), "┏━╸┏━┓") || !strings.Contains(out.String(), "v1") {
		t.Errorf("banner missing: %q", out.String())
	}
}

func TestRender(t *testing.T) {
	got := render("Use `ls`.\n```go\nx := 1\n```\nerror: boom\n[code | m | 1.0s | 3 tok]")
	lines := strings.Split(got, "\n")
	if !strings.Contains(lines[0], cyan+"`ls`"+reset) {
		t.Errorf("inline code: %q", lines[0])
	}
	if !strings.HasPrefix(lines[2], cyan) || !strings.HasPrefix(lines[4], red) || !strings.HasPrefix(lines[5], gray+dim) {
		t.Errorf("render = %q", lines)
	}
}

func TestColorSpinnerStopsCleanly(t *testing.T) {
	var out syncBuffer
	c := NewCLI(strings.NewReader("y\n"), &out)
	c.Color = true
	done := c.Busy("cli")
	time.Sleep(250 * time.Millisecond)
	ans, err := c.Ask(context.Background(), "cli", "overwrite a? [y/N]")
	if err != nil || ans != "y" {
		t.Fatalf("ask = %q, %v", ans, err)
	}
	time.Sleep(150 * time.Millisecond)
	done()
	n := out.Len()
	time.Sleep(200 * time.Millisecond)
	if out.Len() != n {
		t.Error("spinner kept drawing after done()")
	}
	s := out.String()
	if !strings.Contains(s, "…") || !strings.Contains(s, "overwrite a? [y/N]") {
		t.Errorf("out = %q", s)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) Len() int       { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Len() }
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }
