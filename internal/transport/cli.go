package transport

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// CLI reads one message per line from an input stream and writes replies to
// an output stream. All messages belong to a single chat. With Color set it
// draws a banner, a colored prompt, a spinner while a turn runs, and colored
// replies.
type CLI struct {
	ChatID string
	Prompt string
	Color  bool
	out    io.Writer
	lines  chan lineOrErr

	mu   sync.Mutex // guards out and the spinner
	spin *spinner
}

type lineOrErr struct {
	line string
	err  error
}

// NewCLI returns a CLI transport over in and out, without color.
func NewCLI(in io.Reader, out io.Writer) *CLI {
	c := &CLI{ChatID: "cli", Prompt: "> ", out: out, lines: make(chan lineOrErr)}
	go c.read(in)
	return c
}

// NewStdio returns a CLI transport on stdin and stdout, with color when
// stdout is a terminal.
func NewStdio() *CLI {
	c := NewCLI(os.Stdin, os.Stdout)
	if isTerminal(os.Stdout) {
		c.Color = true
		c.Prompt = accent + bold + "❯ " + reset
	}
	return c
}

func (c *CLI) read(in io.Reader) {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		c.lines <- lineOrErr{line: sc.Text()}
	}
	err := sc.Err()
	if err == nil {
		err = io.EOF
	}
	c.lines <- lineOrErr{err: err}
	close(c.lines)
}

func (c *CLI) write(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprint(c.out, s)
}

// Banner prints the start-up banner.
func (c *CLI) Banner(version string) {
	c.write(banner(c.Color, version) + "\n")
}

// Note prints a dim informational line.
func (c *CLI) Note(s string) {
	if c.Color {
		s = gray + s + reset
	}
	c.write("  " + s + "\n\n")
}

// Receive prints the prompt and waits for the next non-empty line.
func (c *CLI) Receive(ctx context.Context) (Message, error) {
	for {
		c.write(c.Prompt)
		select {
		case <-ctx.Done():
			return Message{}, ctx.Err()
		case l, ok := <-c.lines:
			if !ok {
				return Message{}, io.EOF
			}
			if l.err != nil {
				c.write("\n")
				return Message{}, l.err
			}
			switch strings.TrimSpace(l.line) {
			case "":
				continue
			case "/exit", "/quit":
				// Ends this terminal session only; other transports never see it.
				return Message{}, io.EOF
			}
			return Message{ChatID: c.ChatID, Sender: "local", Text: l.line}, nil
		}
	}
}

// Send writes text followed by a blank line.
func (c *CLI) Send(_ context.Context, _ string, text string) error {
	if c.Color {
		text = render(text)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := fmt.Fprintf(c.out, "%s\n\n", text)
	return err
}

// Busy shows a spinner until the returned function is called. Without color
// it does nothing.
func (c *CLI) Busy(_ string) (done func()) {
	if !c.Color {
		return func() {}
	}
	c.mu.Lock()
	c.startSpinLocked()
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		c.stopSpinLocked()
		c.mu.Unlock()
	}
}

// Ask prints a question and waits for the next line, for approval prompts.
// It works because the CLI handles one message at a time: while a turn runs,
// Receive isn't reading, so the next line is the answer. When ctx ends first
// (the approval timeout) it says so and returns ctx's error. A running
// spinner pauses while the question is open.
func (c *CLI) Ask(ctx context.Context, _ string, question string) (string, error) {
	c.mu.Lock()
	wasSpinning := c.stopSpinLocked()
	if c.Color {
		fmt.Fprintf(c.out, "%s? %s%s ", yellow+bold, question, reset)
	} else {
		fmt.Fprintf(c.out, "%s ", question)
	}
	c.mu.Unlock()
	defer func() {
		if wasSpinning {
			c.mu.Lock()
			c.startSpinLocked()
			c.mu.Unlock()
		}
	}()

	select {
	case <-ctx.Done():
		c.write("\n(no answer: treated as no)\n")
		return "", ctx.Err()
	case l, ok := <-c.lines:
		if !ok {
			return "", io.EOF
		}
		if l.err != nil {
			c.write("\n")
			return "", l.err
		}
		return l.line, nil
	}
}

// spinner draws "⠋ Pontificating… 3s" on one line, changing the verb every
// few seconds. All drawing happens under CLI.mu.
type spinner struct {
	stop chan struct{}
	done chan struct{}
}

func (c *CLI) startSpinLocked() {
	if c.spin != nil {
		return
	}
	s := &spinner{stop: make(chan struct{}), done: make(chan struct{})}
	c.spin = s
	go func() {
		defer close(s.done)
		start := time.Now()
		verb := randomVerb("")
		tick := time.NewTicker(90 * time.Millisecond)
		defer tick.Stop()
		for i := 0; ; i++ {
			elapsed := time.Since(start)
			if i > 0 && i%45 == 0 { // about every 4 s
				verb = randomVerb(verb)
			}
			frame := spinFrames[i%len(spinFrames)]
			c.mu.Lock()
			select {
			case <-s.stop:
				c.mu.Unlock()
				return
			default:
			}
			fmt.Fprintf(c.out, "\r\033[K%s%c%s %s%s…%s %s%.0fs%s",
				accent, frame, reset, accent2+italic, verb, reset, gray, elapsed.Seconds(), reset)
			c.mu.Unlock()
			select {
			case <-s.stop:
				return
			case <-tick.C:
			}
		}
	}()
}

// stopSpinLocked stops the spinner and clears its line. It reports whether a
// spinner was running. The caller holds c.mu; the spinner goroutine checks
// stop under the same lock, so it can't draw after this returns.
func (c *CLI) stopSpinLocked() bool {
	s := c.spin
	if s == nil {
		return false
	}
	c.spin = nil
	close(s.stop)
	fmt.Fprint(c.out, "\r\033[K")
	return true
}

// Name implements Transport.
func (c *CLI) Name() string { return "cli" }

// Sequential marks the CLI as handling one message at a time, so Ask can
// read the answer from the same input.
func (c *CLI) Sequential() {}
