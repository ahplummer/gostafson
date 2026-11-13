package projects

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// conceptEmbedder is a fake embedding model: each dimension is a concept,
// and a text scores on it when it contains any of the concept's words. So
// "garage circuit" and "shore power" land near each other without sharing a
// word, the way a real embedding model places them.
type conceptEmbedder struct{ queries int }

var concepts = [][]string{
	{"shore", "garage", "circuit", "amp", "plug", "outlet", "breaker", "30a"},
	{"oil", "5w-20", "engine", "lubricant"},
	{"water", "tank", "fresh", "gallon"},
}

func (e *conceptEmbedder) Embed(_ context.Context, texts []string, query bool) ([][]float32, error) {
	if query {
		e.queries++
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		t = strings.ToLower(t)
		v := make([]float32, len(concepts)+1)
		v[len(concepts)] = 0.01 // never all zero
		for d, words := range concepts {
			for _, w := range words {
				if strings.Contains(t, w) {
					v[d]++
				}
			}
		}
		out[i] = v
	}
	return out, nil
}

func newStore(t *testing.T) *Store {
	t.Helper()
	// The OCR helper takes about a minute to compile; keep it between runs.
	s, err := NewStore(t.TempDir(), &Extractor{CacheDir: filepath.Join(os.TempDir(), "gostafson-test-ocr")}, &conceptEmbedder{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// pdfOf makes a PDF from text with macOS's cupsfilter (one page per form feed).
func pdfOf(t *testing.T, text string) []byte {
	t.Helper()
	if _, err := os.Stat("/usr/sbin/cupsfilter"); err != nil {
		t.Skip("no cupsfilter")
	}
	if _, err := os.Stat(PDFToText); err != nil {
		t.Skip("no pdftotext")
	}
	in := filepath.Join(t.TempDir(), "in.txt")
	os.WriteFile(in, []byte(text), 0o600)
	out, err := exec.Command("/usr/sbin/cupsfilter", "-m", "application/pdf", in).Output()
	if err != nil {
		t.Skipf("cupsfilter: %v", err)
	}
	return out
}

func TestProjectBasics(t *testing.T) {
	s := newStore(t)
	if _, err := s.Create("Bad Name"); err == nil {
		t.Error("bad name accepted")
	}
	p, err := s.Create("travato")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("travato"); err == nil {
		t.Error("created twice")
	}
	if names, _ := s.List(); len(names) != 1 || names[0] != "travato" {
		t.Errorf("list = %v", names)
	}
	if _, err := s.Open("nope"); err == nil {
		t.Error("opened a missing project")
	}
	if st, _ := p.Settings(); st.Cloud {
		t.Error("cloud on by default")
	}
	if err := p.SetCloud(true); err != nil {
		t.Fatal(err)
	}
	if st, _ := p.Settings(); !st.Cloud {
		t.Error("cloud on not saved")
	}
	if b, _ := os.ReadFile(filepath.Join(p.Dir, "project.toml")); !strings.Contains(string(b), "cloud = true") {
		t.Errorf("project.toml = %s", b)
	}
	p.SetCloud(false)
	if st, _ := p.Settings(); st.Cloud {
		t.Error("cloud off not saved")
	}
	if _, ok := p.Tools()["remember"]; !ok {
		t.Error("no remember tool")
	}
	if err := p.Remember("2023 Travato 59K on a Ram ProMaster 3500"); err != nil {
		t.Fatal(err)
	}
	ctx := p.Context(context.Background(), true)
	for _, want := range []string{`project "travato"`, "Instructions and standing context", "ProMaster 3500", "project_search"} {
		if !strings.Contains(ctx, want) {
			t.Errorf("context lacks %q:\n%s", want, ctx)
		}
	}
	if _, err := p.AddSource("../../etc/passwd.pdf", []byte("x")); err != nil {
		t.Fatal(err) // reduced to its base name
	}
	if _, err := os.Stat(filepath.Join(p.Dir, "sources", "passwd.pdf")); err != nil {
		t.Error("source not saved under its base name")
	}
	if _, err := p.AddSource("tool.exe", []byte("x")); err == nil {
		t.Error("an .exe accepted as a source")
	}
}

func TestIndexAndSearch(t *testing.T) {
	s := newStore(t)
	p, _ := s.Create("travato")
	ctx := context.Background()
	manual := pdfOf(t, "Engine oil: use SAE 5W-20 synthetic.\nCheck the level every fuel stop.\f"+
		"Shore power: the coach has a 30A TT-30 inlet. Connect the cord with the supplied adapter "+
		"and limit the load: do not run the air conditioner on a 15A or 20A supply.\f")
	p.AddSource("Owner's Manual.pdf", manual)
	p.AddSource("notes.md", []byte("# Notes\nFresh water tank holds 21 gallons.\n"))
	rep, err := p.Index.Sync(ctx, nil)
	if err != nil || len(rep.Added) != 2 || len(rep.Failed) != 0 {
		t.Fatalf("sync = %+v, %v", rep, err)
	}

	hits, _ := p.Index.Search(ctx, "what kind of oil should I use?", 3)
	if len(hits) == 0 || hits[0].Source != "Owner's Manual.pdf" || hits[0].Page != 1 || !strings.Contains(hits[0].Text, "5W-20") {
		t.Errorf("oil hits = %+v", hits)
	}
	// No word in common with the manual's page ("plug", "garage"): meaning
	// search finds the shore power page.
	hits, _ = p.Index.Search(ctx, "can I plug into my garage", 3)
	if len(hits) == 0 || hits[0].Page != 2 || !strings.Contains(hits[0].Text, "TT-30") {
		t.Errorf("garage hits = %+v", hits)
	}
	if got := p.Passages(ctx, "what oil should I use?"); !strings.Contains(got, "(Owner's Manual.pdf p. 1)") || !strings.Contains(got, "5W-20") {
		t.Errorf("passages = %q", got)
	}
	text, pages, err := p.Index.ReadPage(ctx, "Owner's Manual.pdf", 2)
	if err != nil || pages != 2 || !strings.Contains(text, "adapter") {
		t.Errorf("read page 2 = %q, %d, %v", text, pages, err)
	}
	if _, _, err := p.Index.ReadPage(ctx, "Owner's Manual.pdf", 9); err == nil {
		t.Error("page 9 of 2: no error")
	}

	// Nothing changed: nothing reindexed. A removed file leaves the index.
	if rep, _ := p.Index.Sync(ctx, nil); rep.Changed() {
		t.Errorf("second sync changed %+v", rep)
	}
	os.Remove(filepath.Join(p.Dir, "sources", "notes.md"))
	if rep, _ := p.Index.Sync(ctx, nil); len(rep.Removed) != 1 {
		t.Errorf("removal = %+v", rep)
	}
	if hits, _ := p.Index.Search(ctx, "fresh water gallons", 3); len(hits) > 0 && hits[0].Source == "notes.md" {
		t.Error("removed source still found")
	}
	// FTS5 syntax in a question is just text.
	if _, err := p.Index.Search(ctx, `oil" OR NEAR(* -- ")`, 3); err != nil {
		t.Errorf("odd query: %v", err)
	}
}

// A scanned page (an image in a PDF) and a screenshot are read with OCR.
func TestOCR(t *testing.T) {
	if _, err := os.Stat(SwiftC); err != nil {
		t.Skip("no swiftc")
	}
	s := newStore(t)
	p, _ := s.Create("scans")
	ctx := context.Background()
	text := pdfOf(t, "MAXIMUM BRANCH CIRCUIT 30 AMPS\nDO NOT EXCEED\n")
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "label.pdf"), text, 0o600)
	// Render the page to PNG (a "screenshot"), then wrap that image in a PDF (a scan).
	if out, err := exec.Command("/usr/bin/sips", "-s", "format", "png", filepath.Join(dir, "label.pdf"), "--out", filepath.Join(dir, "label.png")).CombinedOutput(); err != nil {
		t.Skipf("sips: %v %s", err, out)
	}
	exec.Command("/usr/bin/sips", "-s", "format", "pdf", filepath.Join(dir, "label.png"), "--out", filepath.Join(dir, "scan.pdf")).Run()
	png, _ := os.ReadFile(filepath.Join(dir, "label.png"))
	scan, _ := os.ReadFile(filepath.Join(dir, "scan.pdf"))
	p.AddSource("facebook screenshot.png", png)
	p.AddSource("scanned label.pdf", scan)
	rep, err := p.Index.Sync(ctx, nil)
	if err != nil || len(rep.Failed) > 0 {
		t.Fatalf("sync = %+v, %v", rep, err)
	}
	for _, src := range []string{"facebook screenshot.png", "scanned label.pdf"} {
		page := 0
		if strings.HasSuffix(src, ".pdf") {
			page = 1
		}
		got, _, err := p.Index.ReadPage(ctx, src, page)
		if err != nil || !strings.Contains(strings.ToUpper(got), "BRANCH CIRCUIT") {
			t.Errorf("%s OCR = %q, %v", src, got, err)
		}
	}
}

func TestChunks(t *testing.T) {
	long := strings.Repeat("A sentence about the water heater. ", 100) // 3,500 characters
	cs := Chunks([]Page{{Number: 3, Text: long}, {Number: 4, Text: "short"}})
	if len(cs) < 3 || cs[0].Page != 3 || cs[len(cs)-1].Text != "short" || cs[len(cs)-1].Page != 4 {
		t.Fatalf("chunks = %d, last %+v", len(cs), cs[len(cs)-1])
	}
	for _, c := range cs {
		if len(c.Text) > chunkSize {
			t.Errorf("chunk of %d characters", len(c.Text))
		}
	}
}
