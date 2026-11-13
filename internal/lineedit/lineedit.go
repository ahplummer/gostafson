// Package lineedit is a small line editor for the CLI, written for
// gostafson instead of taken as a dependency. It reads keys in raw mode and
// supports: cursor movement and word/line deletion (Emacs keys and arrows),
// history (up/down), multi-line paste (bracketed paste), a dim completion
// hint for "/" commands and "@" tags (Tab accepts it, or lists the choices),
// and Ctrl+V, which inserts a clipboard image as "[image #1]" (or pastes the
// clipboard's text); ⌘V does the same where the terminal sends an empty
// paste for an image-only clipboard. Image files dragged into the terminal arrive as their
// paths and are attached on Enter.
package lineedit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ahplummer/gostafson/internal/media"
)

// ErrInterrupt is returned when Ctrl-C is pressed on an empty line twice.
var ErrInterrupt = errors.New("interrupted")

// Editor reads lines. Out receives all drawing; Lock, if set, is held while
// drawing so other output (the spinner) can't interleave.
type Editor struct {
	In        <-chan []byte // raw bytes from the terminal
	Out       io.Writer
	Lock      sync.Locker
	Raw       func() (restore func(), err error) // nil: assume raw already
	Width     func() int
	Clipboard func(ctx context.Context) (image []byte, text string, err error)
	// Complete returns candidates for the word at the start of the line
	// ("/he" -> "/help"; "@co" -> "@code"); nil disables hints.
	Complete func(word string) []string

	history []string
	pending []byte // bytes read but not yet decoded
}

// Line is one submitted line and what is attached to it.
type Line struct {
	Text   string
	Images []media.Image
	Files  []media.Image // documents (PDF, text) dragged in, for a project
}

// Options for one ReadLine call.
type Options struct {
	History  bool // up/down browse, and the line is added to history
	Complete bool // show completion hints
}

// state is the line being edited.
type state struct {
	buf      []rune
	pos      int
	images   map[int]media.Image // "[image #n]" -> image
	nextImg  int
	hist     int    // index into history while browsing; len(history) = the new line
	saved    []rune // the new line while browsing history
	row      int    // terminal row of the cursor, relative to the prompt's row
	prompt   string
	pw       int // prompt's visible width
	lastCtlC time.Time
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// ReadLine shows prompt and returns the line when Enter is pressed. Ctrl-D on
// an empty line returns io.EOF.
func (e *Editor) ReadLine(ctx context.Context, prompt string, opt Options) (Line, error) {
	if e.Raw != nil {
		restore, err := e.Raw()
		if err != nil {
			return Line{}, err
		}
		defer restore()
	}
	e.write("\x1b[?2004h") // bracketed paste on: a pasted block arrives as one unit
	defer e.write("\x1b[?2004l")
	s := &state{prompt: prompt, pw: textWidth(ansi.ReplaceAllString(prompt, "")), images: map[int]media.Image{}, hist: len(e.history)}
	e.redraw(s, opt)
	for {
		k, err := e.key(ctx)
		if err != nil {
			e.write("\r\n")
			return Line{}, err
		}
		done, err := e.apply(ctx, s, k, opt)
		if err != nil {
			e.finish(s)
			return Line{}, err
		}
		if done {
			e.finish(s)
			line := e.submit(s)
			if opt.History && strings.TrimSpace(line.Text) != "" && (len(e.history) == 0 || e.history[len(e.history)-1] != string(s.buf)) {
				e.history = append(e.history, string(s.buf))
			}
			return line, nil
		}
		e.redraw(s, opt)
	}
}

// key kinds beyond plain runes.
const (
	kRune = iota
	kEnter
	kBackspace
	kDelete
	kLeft
	kRight
	kUp
	kDown
	kHome
	kEnd
	kWordLeft
	kWordRight
	kKillWordBack
	kKillToEnd
	kKillToStart
	kTab
	kCtrlC
	kCtrlD
	kCtrlL
	kCtrlV
	kPaste
	kIgnore
)

type key struct {
	kind int
	r    rune
	text string // kPaste
}

func (e *Editor) byte(ctx context.Context, wait time.Duration) (byte, bool, error) {
	if len(e.pending) == 0 {
		var timeout <-chan time.Time
		if wait > 0 {
			timeout = time.After(wait)
		}
		select {
		case <-ctx.Done():
			return 0, false, ctx.Err()
		case b, ok := <-e.In:
			if !ok {
				return 0, false, io.EOF
			}
			e.pending = append(e.pending, b...)
		case <-timeout:
			return 0, false, nil
		}
		if len(e.pending) == 0 {
			return e.byte(ctx, wait)
		}
	}
	b := e.pending[0]
	e.pending = e.pending[1:]
	return b, true, nil
}

// key decodes the next key from the byte stream.
func (e *Editor) key(ctx context.Context) (key, error) {
	b, _, err := e.byte(ctx, 0)
	if err != nil {
		return key{}, err
	}
	switch b {
	case '\r', '\n':
		return key{kind: kEnter}, nil
	case 0x7f, 0x08:
		return key{kind: kBackspace}, nil
	case 0x01:
		return key{kind: kHome}, nil
	case 0x05:
		return key{kind: kEnd}, nil
	case 0x02:
		return key{kind: kLeft}, nil
	case 0x06:
		return key{kind: kRight}, nil
	case 0x0b:
		return key{kind: kKillToEnd}, nil
	case 0x15:
		return key{kind: kKillToStart}, nil
	case 0x17:
		return key{kind: kKillWordBack}, nil
	case 0x09:
		return key{kind: kTab}, nil
	case 0x03:
		return key{kind: kCtrlC}, nil
	case 0x04:
		return key{kind: kCtrlD}, nil
	case 0x0c:
		return key{kind: kCtrlL}, nil
	case 0x16:
		return key{kind: kCtrlV}, nil
	case 0x10:
		return key{kind: kUp}, nil
	case 0x0e:
		return key{kind: kDown}, nil
	case 0x1b:
		return e.escape(ctx)
	}
	if b < 0x20 {
		return key{kind: kIgnore}, nil
	}
	// UTF-8: collect the continuation bytes.
	buf := []byte{b}
	for !utf8.FullRune(buf) && len(buf) < utf8.UTFMax {
		c, ok, err := e.byte(ctx, 50*time.Millisecond)
		if err != nil {
			return key{}, err
		}
		if !ok {
			break
		}
		buf = append(buf, c)
	}
	r, _ := utf8.DecodeRune(buf)
	return key{kind: kRune, r: r}, nil
}

func (e *Editor) escape(ctx context.Context) (key, error) {
	b, ok, err := e.byte(ctx, 30*time.Millisecond)
	if err != nil || !ok {
		return key{kind: kIgnore}, err // a lone Esc
	}
	switch b {
	case 'b':
		return key{kind: kWordLeft}, nil
	case 'f':
		return key{kind: kWordRight}, nil
	case 0x7f:
		return key{kind: kKillWordBack}, nil
	case 'O':
		c, _, err := e.byte(ctx, 30*time.Millisecond)
		return csiFinal(c, ""), err
	case '[':
	default:
		return key{kind: kIgnore}, nil
	}
	var params []byte
	for {
		c, ok, err := e.byte(ctx, 30*time.Millisecond)
		if err != nil || !ok {
			return key{kind: kIgnore}, err
		}
		if c >= 0x40 && c <= 0x7e {
			if c == '~' && string(params) == "200" {
				return e.paste(ctx)
			}
			return csiFinal(c, string(params)), nil
		}
		params = append(params, c)
	}
}

func csiFinal(c byte, params string) key {
	switch c {
	case 'A':
		return key{kind: kUp}
	case 'B':
		return key{kind: kDown}
	case 'C':
		if strings.HasSuffix(params, ";3") || strings.HasSuffix(params, ";5") {
			return key{kind: kWordRight}
		}
		return key{kind: kRight}
	case 'D':
		if strings.HasSuffix(params, ";3") || strings.HasSuffix(params, ";5") {
			return key{kind: kWordLeft}
		}
		return key{kind: kLeft}
	case 'H':
		return key{kind: kHome}
	case 'F':
		return key{kind: kEnd}
	case '~':
		switch params {
		case "1", "7":
			return key{kind: kHome}
		case "4", "8":
			return key{kind: kEnd}
		case "3":
			return key{kind: kDelete}
		}
	}
	return key{kind: kIgnore}
}

// paste reads a bracketed paste up to ESC [ 201 ~.
func (e *Editor) paste(ctx context.Context) (key, error) {
	const end = "\x1b[201~"
	var buf []byte
	for !strings.HasSuffix(string(buf), end) {
		c, ok, err := e.byte(ctx, 2*time.Second)
		if err != nil {
			return key{}, err
		}
		if !ok {
			break // an unterminated paste: take what came
		}
		buf = append(buf, c)
	}
	t := strings.TrimSuffix(string(buf), end)
	t = strings.ReplaceAll(strings.ReplaceAll(t, "\r\n", "\n"), "\r", "\n")
	return key{kind: kPaste, text: t}, nil
}

// apply edits the state for one key; done means Enter was pressed.
func (e *Editor) apply(ctx context.Context, s *state, k key, opt Options) (bool, error) {
	if k.kind != kCtrlC {
		s.lastCtlC = time.Time{}
	}
	switch k.kind {
	case kRune:
		s.insert([]rune{k.r})
	case kPaste:
		if k.text == "" {
			// ⌘V with only an image on the clipboard: terminals that use
			// bracketed paste send an empty paste. Look for the image.
			e.pasteClipboard(ctx, s)
			break
		}
		s.insert([]rune(k.text))
	case kEnter:
		return true, nil
	case kBackspace:
		if s.pos > 0 {
			s.buf = slices.Delete(s.buf, s.pos-1, s.pos)
			s.pos--
		}
	case kDelete:
		if s.pos < len(s.buf) {
			s.buf = slices.Delete(s.buf, s.pos, s.pos+1)
		}
	case kLeft:
		s.pos = max(0, s.pos-1)
	case kRight:
		if s.pos == len(s.buf) && opt.Complete {
			s.acceptHint(e)
		}
		s.pos = min(len(s.buf), s.pos+1)
	case kHome:
		s.pos = 0
	case kEnd:
		s.pos = len(s.buf)
	case kWordLeft:
		s.pos = wordStart(s.buf, s.pos)
	case kWordRight:
		s.pos = wordEnd(s.buf, s.pos)
	case kKillWordBack:
		i := wordStart(s.buf, s.pos)
		s.buf = slices.Delete(s.buf, i, s.pos)
		s.pos = i
	case kKillToEnd:
		s.buf = s.buf[:s.pos]
	case kKillToStart:
		s.buf = slices.Delete(s.buf, 0, s.pos)
		s.pos = 0
	case kUp, kDown:
		if opt.History {
			e.browse(s, k.kind == kUp)
		}
	case kTab:
		if opt.Complete {
			e.tab(s)
		}
	case kCtrlL:
		e.write("\x1b[H\x1b[2J")
		s.row = 0
	case kCtrlD:
		if len(s.buf) == 0 {
			return false, io.EOF
		}
		if s.pos < len(s.buf) {
			s.buf = slices.Delete(s.buf, s.pos, s.pos+1)
		}
	case kCtrlC:
		if len(s.buf) > 0 {
			s.buf, s.pos = nil, 0
			return false, nil
		}
		if !s.lastCtlC.IsZero() && time.Since(s.lastCtlC) < 2*time.Second {
			return false, ErrInterrupt
		}
		s.lastCtlC = time.Now()
		e.note(s, "press Ctrl-C again to quit, or type /exit")
	case kCtrlV:
		e.pasteClipboard(ctx, s)
	}
	return false, nil
}

func (s *state) insert(rs []rune) {
	s.buf = slices.Insert(s.buf, s.pos, rs...)
	s.pos += len(rs)
}

func (e *Editor) pasteClipboard(ctx context.Context, s *state) {
	if e.Clipboard == nil {
		return
	}
	img, text, err := e.Clipboard(ctx)
	switch {
	case err != nil:
		e.note(s, err.Error())
	case img != nil:
		s.nextImg++
		s.images[s.nextImg] = media.Image{Name: fmt.Sprintf("clipboard-%d.png", s.nextImg), Data: img}
		s.insert([]rune(fmt.Sprintf("[image #%d]", s.nextImg)))
	case text != "":
		s.insert([]rune(strings.ReplaceAll(text, "\r\n", "\n")))
	default:
		e.note(s, "the clipboard is empty")
	}
}

func (e *Editor) browse(s *state, up bool) {
	if len(e.history) == 0 {
		return
	}
	if s.hist == len(e.history) {
		s.saved = slices.Clone(s.buf)
	}
	switch {
	case up && s.hist > 0:
		s.hist--
	case !up && s.hist < len(e.history):
		s.hist++
	default:
		return
	}
	if s.hist == len(e.history) {
		s.buf = slices.Clone(s.saved)
	} else {
		s.buf = []rune(e.history[s.hist])
	}
	s.pos = len(s.buf)
}

// word returns the completion word: the line so far, when it is a single
// word starting with "/" or "@" and the cursor is at its end.
func (s *state) word() string {
	if s.pos != len(s.buf) {
		return ""
	}
	w := string(s.buf)
	if w == "" || strings.ContainsAny(w, " \n\t") || (w[0] != '/' && w[0] != '@') {
		return ""
	}
	return w
}

func (e *Editor) candidates(s *state) []string {
	w := s.word()
	if w == "" || e.Complete == nil {
		return nil
	}
	var out []string
	for _, c := range e.Complete(w) {
		if strings.HasPrefix(c, w) && c != w {
			out = append(out, c)
		}
	}
	return out
}

// hint is the dim text shown after the cursor: the rest of the first
// candidate.
func (s *state) hint(e *Editor) string {
	if c := e.candidates(s); len(c) > 0 {
		return strings.TrimPrefix(c[0], s.word())
	}
	return ""
}

func (s *state) acceptHint(e *Editor) {
	if h := s.hint(e); h != "" {
		s.insert([]rune(h))
	}
}

func (e *Editor) tab(s *state) {
	c := e.candidates(s)
	switch {
	case len(c) == 1:
		s.insert([]rune(strings.TrimPrefix(c[0], s.word()) + " "))
	case len(c) > 1:
		if p := commonPrefix(c); len(p) > len(s.word()) {
			s.insert([]rune(strings.TrimPrefix(p, s.word())))
			return
		}
		e.note(s, strings.Join(c, "  "))
	}
}

func commonPrefix(c []string) string {
	p := c[0]
	for _, s := range c[1:] {
		for !strings.HasPrefix(s, p) {
			p = p[:len(p)-1]
		}
	}
	return p
}

// note prints a dim line below the input and redraws the input under it.
func (e *Editor) note(s *state, msg string) {
	e.finish(s)
	e.write("\x1b[2m  " + msg + "\x1b[0m\r\n")
	s.row = 0
}

// submit builds the line: clipboard images whose "[image #n]" is still in
// the text, and image files whose paths are in it (a file dragged into the
// terminal arrives as its path).
func (e *Editor) submit(s *state) Line {
	text := string(s.buf)
	var ims []media.Image
	for n := 1; n <= s.nextImg; n++ {
		if strings.Contains(text, fmt.Sprintf("[image #%d]", n)) {
			ims = append(ims, s.images[n])
		}
	}
	text, dropped, docs := ImagePaths(text, s.nextImg)
	return Line{Text: text, Images: append(ims, dropped...), Files: docs}
}

var pathToken = regexp.MustCompile(`'[^']+'|"[^"]+"|(?:\\ |[^\s'"])+`)

// ImagePaths finds paths of existing image and document files in text,
// reads them, and replaces each path with "[image #n]" (numbering after the
// n images already attached) or "[file: name]". Terminals quote a dropped
// path or escape its spaces.
func ImagePaths(text string, n int) (string, []media.Image, []media.Image) {
	var ims, docs []media.Image
	out := pathToken.ReplaceAllStringFunc(text, func(tok string) string {
		p := strings.Trim(tok, `'"`)
		p = strings.ReplaceAll(p, `\ `, " ")
		if strings.HasPrefix(p, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				p = filepath.Join(home, p[2:])
			}
		}
		doc := false
		switch strings.ToLower(filepath.Ext(p)) {
		case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".heic":
		case ".pdf", ".txt", ".md", ".markdown", ".csv":
			doc = true
		default:
			return tok
		}
		if !filepath.IsAbs(p) {
			return tok
		}
		info, err := os.Stat(p)
		if err != nil || !info.Mode().IsRegular() || info.Size() > media.MaxBytes {
			return tok
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return tok
		}
		if doc {
			docs = append(docs, media.Image{Name: filepath.Base(p), Data: data})
			return "[file: " + filepath.Base(p) + "]"
		}
		ims = append(ims, media.Image{Name: filepath.Base(p), Data: data})
		return fmt.Sprintf("[image #%d]", n+len(ims))
	})
	return out, ims, docs
}

// --- drawing ---

func (e *Editor) write(s string) {
	if e.Lock != nil {
		e.Lock.Lock()
		defer e.Lock.Unlock()
	}
	io.WriteString(e.Out, s)
}

func (e *Editor) width() int {
	if e.Width != nil {
		if w := e.Width(); w > 0 {
			return w
		}
	}
	return 80
}

// layout returns the (row, col) after drawing runes from (row, col).
func layout(rs []rune, row, col, width, indent int) (int, int) {
	for _, r := range rs {
		if r == '\n' {
			row, col = row+1, indent
			continue
		}
		w := runeWidth(r)
		if col+w > width {
			row, col = row+1, 0
		}
		col += w
	}
	return row, col
}

// redraw draws the prompt and line from the prompt's row, then puts the
// cursor at pos. Continuation lines are indented under the prompt.
func (e *Editor) redraw(s *state, opt Options) {
	width := e.width()
	hint := ""
	if opt.Complete {
		hint = s.hint(e)
	}
	var b strings.Builder
	if s.row > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", s.row)
	}
	b.WriteString("\r\x1b[J")
	b.WriteString(s.prompt)
	indent := strings.Repeat(" ", min(s.pw, width-1))
	b.WriteString(strings.ReplaceAll(string(s.buf), "\n", "\r\n"+indent))
	if hint != "" {
		b.WriteString("\x1b[2m" + hint + "\x1b[0m")
	}
	r0, c0 := s.pw/width, s.pw%width // a prompt longer than a line wraps too
	endRow, endCol := layout(append(slices.Clone(s.buf), []rune(hint)...), r0, c0, width, len(indent))
	if endCol >= width { // the cursor sits past the last column: make the wrap real
		b.WriteString("\r\n")
		endRow, endCol = endRow+1, 0
	}
	curRow, curCol := layout(s.buf[:s.pos], r0, c0, width, len(indent))
	if curCol >= width {
		curRow, curCol = curRow+1, 0
	}
	if up := endRow - curRow; up > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", up)
	}
	b.WriteString("\r")
	if curCol > 0 {
		fmt.Fprintf(&b, "\x1b[%dC", curCol)
	}
	s.row = curRow
	e.write(b.String())
}

// finish moves the cursor below the line, leaving it as typed.
func (e *Editor) finish(s *state) {
	indent := min(s.pw, e.width()-1)
	var b strings.Builder
	if s.row > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", s.row)
	}
	b.WriteString("\r\x1b[J" + s.prompt + strings.ReplaceAll(string(s.buf), "\n", "\r\n"+strings.Repeat(" ", indent)) + "\r\n")
	s.row = 0
	e.write(b.String())
}

func runeWidth(r rune) int {
	switch {
	case r < 0x20 || unicode.Is(unicode.Mn, r):
		return 0
	case unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hangul, r) || unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) || (r >= 0x1f300 && r <= 0x1faff) || (r >= 0xff01 && r <= 0xff60):
		return 2
	}
	return 1
}

func textWidth(s string) int {
	n := 0
	for _, r := range s {
		n += runeWidth(r)
	}
	return n
}

func isWord(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }

func wordStart(buf []rune, pos int) int {
	i := pos
	for i > 0 && !isWord(buf[i-1]) {
		i--
	}
	for i > 0 && isWord(buf[i-1]) {
		i--
	}
	return i
}

func wordEnd(buf []rune, pos int) int {
	i := pos
	for i < len(buf) && !isWord(buf[i]) {
		i++
	}
	for i < len(buf) && isWord(buf[i]) {
		i++
	}
	return i
}
