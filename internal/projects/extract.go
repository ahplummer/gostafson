package projects

import (
	"bufio"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Page is the text of one page of a source (Page 0 for sources without
// pages: text files and images).
type Page struct {
	Number int
	Text   string
}

// Tool locations. Fixed paths, no PATH lookup: these run on the host as the
// agent user, with arguments built here, never by a model.
var (
	PDFToText = "/opt/homebrew/bin/pdftotext"
	SwiftC    = "/usr/bin/swiftc"
)

// minPageText: a PDF page with less text than this is treated as scanned
// and read with OCR.
const minPageText = 40

// Extractor turns source files into page text.
type Extractor struct {
	CacheDir string // where the OCR helper is compiled
	// Describe, if set, describes an image in a sentence or two (a vision
	// model), so a screenshot is found by what it shows, not only its text.
	Describe func(ctx context.Context, path string) (string, error)

	once   sync.Once
	ocr    string
	ocrErr error
}

//go:embed ocr.swift
var ocrSource string

// Kind returns the source kind by extension: "pdf", "text", "image", or "".
func Kind(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".pdf":
		return "pdf"
	case ".md", ".txt", ".markdown", ".csv":
		return "text"
	case ".png", ".jpg", ".jpeg", ".heic", ".gif", ".webp":
		return "image"
	}
	return ""
}

// Extract reads a source's text, page by page.
func (x *Extractor) Extract(ctx context.Context, path string) ([]Page, error) {
	switch Kind(path) {
	case "text":
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if !utf8.Valid(b) {
			return nil, errors.New("not UTF-8 text")
		}
		return []Page{{Text: string(b)}}, nil
	case "pdf":
		return x.pdf(ctx, path)
	case "image":
		return x.image(ctx, path)
	}
	return nil, fmt.Errorf("%s: only PDF, text and image files can be project sources", filepath.Base(path))
}

func (x *Extractor) pdf(ctx context.Context, path string) ([]Page, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx2, PDFToText, "-layout", "-enc", "UTF-8", path, "-").Output()
	if err != nil {
		return nil, fmt.Errorf("pdftotext: %v", err)
	}
	// pdftotext separates pages with form feeds.
	raw := strings.Split(strings.ToValidUTF8(string(out), ""), "\f")
	if n := len(raw); n > 0 && strings.TrimSpace(raw[n-1]) == "" {
		raw = raw[:n-1]
	}
	pages := make([]Page, len(raw))
	var scanned []string
	for i, t := range raw {
		pages[i] = Page{Number: i + 1, Text: t}
		if len(strings.TrimSpace(t)) < minPageText {
			scanned = append(scanned, fmt.Sprintf("pdf:%d:%s", i+1, path))
		}
	}
	if len(scanned) > 0 {
		texts, err := x.runOCR(ctx, scanned)
		if err != nil && len(scanned) == len(pages) {
			return nil, fmt.Errorf("the PDF has no text layer and OCR failed: %v", err)
		}
		for _, item := range scanned {
			var n int
			fmt.Sscanf(item, "pdf:%d:", &n)
			if t := texts[item]; n > 0 && n <= len(pages) && t != "" {
				pages[n-1].Text = t
			}
		}
	}
	return pages, nil
}

func (x *Extractor) image(ctx context.Context, path string) ([]Page, error) {
	item := "img:" + path
	texts, err := x.runOCR(ctx, []string{item})
	text := texts[item]
	if x.Describe != nil {
		if d, derr := x.Describe(ctx, path); derr == nil && strings.TrimSpace(d) != "" {
			text = "[image: " + strings.TrimSpace(d) + "]\n" + text
		}
	}
	if strings.TrimSpace(text) == "" && err != nil {
		return nil, err
	}
	return []Page{{Text: text}}, nil
}

// runOCR runs the OCR helper over items, compiling it first if needed. It
// returns the text per item.
func (x *Extractor) runOCR(ctx context.Context, items []string) (map[string]string, error) {
	bin, err := x.helper(ctx)
	if err != nil {
		return map[string]string{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, items...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return map[string]string{}, err
	}
	if err := cmd.Start(); err != nil {
		return map[string]string{}, err
	}
	texts := map[string]string{}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var r struct{ Item, Text, Error string }
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.Error == "" {
			texts[r.Item] = r.Text
		}
	}
	return texts, cmd.Wait()
}

// helper compiles ocr.swift into the cache dir, once per source version.
func (x *Extractor) helper(ctx context.Context) (string, error) {
	x.once.Do(func() {
		sum := sha256.Sum256([]byte(ocrSource))
		bin := filepath.Join(x.CacheDir, "ocr-"+hex.EncodeToString(sum[:6]))
		if _, err := os.Stat(bin); err == nil {
			x.ocr = bin
			return
		}
		if err := os.MkdirAll(x.CacheDir, 0o700); err != nil {
			x.ocrErr = err
			return
		}
		src := bin + ".swift"
		if err := os.WriteFile(src, []byte(ocrSource), 0o600); err != nil {
			x.ocrErr = err
			return
		}
		defer os.Remove(src)
		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		tmp := bin + ".partial"
		cmd := exec.CommandContext(ctx, SwiftC, "-O", "-o", tmp, src)
		cmd.Env = append(os.Environ(), "TMPDIR="+x.CacheDir)
		if out, err := cmd.CombinedOutput(); err != nil {
			x.ocrErr = fmt.Errorf("compiling the OCR helper: %v: %s", err, strings.TrimSpace(string(out)))
			return
		}
		x.ocrErr = os.Rename(tmp, bin)
		x.ocr = bin
	})
	return x.ocr, x.ocrErr
}

// Chunk is a passage of a page, the unit of search.
type Chunk struct {
	Page int
	Text string
}

// chunkSize and chunkOverlap are in characters: about 300 tokens a chunk,
// small enough that several fit in a turn, large enough to keep a
// manual's paragraph together.
const (
	chunkSize    = 1200
	chunkOverlap = 200
)

// Chunks splits pages into passages at paragraph or line breaks.
func Chunks(pages []Page) []Chunk {
	var out []Chunk
	for _, p := range pages {
		text := strings.TrimSpace(collapse(p.Text))
		for len(text) > 0 {
			if len(text) <= chunkSize {
				out = append(out, Chunk{Page: p.Number, Text: text})
				break
			}
			cut := chunkSize
			if i := strings.LastIndex(text[:chunkSize], "\n\n"); i > chunkSize/2 {
				cut = i
			} else if i := strings.LastIndex(text[:chunkSize], "\n"); i > chunkSize/2 {
				cut = i
			} else if i := strings.LastIndex(text[:chunkSize], " "); i > chunkSize/2 {
				cut = i
			}
			for cut > 0 && !utf8.RuneStart(text[cut]) {
				cut--
			}
			out = append(out, Chunk{Page: p.Number, Text: strings.TrimSpace(text[:cut])})
			next := max(cut-chunkOverlap, 1)
			for next < len(text) && !utf8.RuneStart(text[next]) {
				next++
			}
			text = strings.TrimSpace(text[next:])
		}
	}
	return out
}

// collapse squeezes the runs of spaces pdftotext -layout uses for columns.
func collapse(s string) string {
	var b strings.Builder
	spaces := 0
	for _, r := range s {
		if r == ' ' || r == '\t' {
			spaces++
			continue
		}
		if spaces > 0 {
			if spaces > 1 && r != '\n' {
				b.WriteString("  ")
			} else if r != '\n' {
				b.WriteByte(' ')
			}
			spaces = 0
		}
		b.WriteRune(r)
	}
	return b.String()
}
