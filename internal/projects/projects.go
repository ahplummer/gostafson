// Package projects keeps long-term context for a topic (PRD D37): a
// directory per project with instructions, a memory file the agent appends
// to, and sources (PDFs, text, screenshots) that are indexed for keyword and
// meaning search. A chat attaches a project with /project; its turns then get
// the instructions and memory in the prompt and search tools over the
// sources.
//
//	<dir>/<name>/
//	  PROJECT.md     instructions and standing context, sent every turn
//	  memory.md      facts saved with the remember tool, sent every turn
//	  project.toml   settings: cloud = true lets cloud profiles search it
//	  sources/       the documents
//	  .index.db      the search index, rebuilt from sources/
package projects

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
)

// Store is the directory holding all projects.
type Store struct {
	Dir      string
	Extract  *Extractor
	Embedder Embedder

	mu   sync.Mutex
	open map[string]*Project
}

// NewStore returns a store over dir (created 0700).
func NewStore(dir string, x *Extractor, e Embedder) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{Dir: dir, Extract: x, Embedder: e, open: map[string]*Project{}}, nil
}

var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// CheckName reports whether name can be a project name: lower-case letters,
// digits and dashes, up to 40 characters.
func CheckName(name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("%q isn't a project name: use lower-case letters, digits and dashes (like travato or relay-app)", name)
	}
	return nil
}

// List returns the project names, sorted.
func (s *Store) List() ([]string, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && validName.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

const projectTemplate = `# %s

Instructions and standing context for this project go here. They are sent
with every message while the project is attached (/project %s), so keep them
short: what the project is about, what matters, how answers should look.
`

// Create makes a new, empty project.
func (s *Store) Create(name string) (*Project, error) {
	if err := CheckName(name); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.Dir, name)
	if _, err := os.Stat(dir); err == nil {
		return nil, fmt.Errorf("project %s already exists", name)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sources"), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "PROJECT.md"), []byte(fmt.Sprintf(projectTemplate, name, name)), 0o600); err != nil {
		return nil, err
	}
	return s.Open(name)
}

// Open returns a project, opening its index once and keeping it.
func (s *Store) Open(name string) (*Project, error) {
	if err := CheckName(name); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.open[name]; ok {
		return p, nil
	}
	dir := filepath.Join(s.Dir, name)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("no project %s (/projects lists them; /project new %s creates it)", name, name)
	}
	os.MkdirAll(filepath.Join(dir, "sources"), 0o700)
	ix, err := OpenIndex(filepath.Join(dir, ".index.db"), filepath.Join(dir, "sources"), s.Extract, s.Embedder)
	if err != nil {
		return nil, err
	}
	p := &Project{Name: name, Dir: dir, Index: ix}
	s.open[name] = p
	return p, nil
}

// Close closes every open index.
func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.open {
		p.Index.Close()
	}
	s.open = map[string]*Project{}
}

// Project is one open project.
type Project struct {
	Name  string
	Dir   string
	Index *Index

	memMu sync.Mutex
}

// Settings are a project's project.toml.
type Settings struct {
	// Cloud lets cloud profiles see the project: its instructions, memory and
	// the read-only search tools. Off by default (PRD D37).
	Cloud bool `toml:"cloud"`
}

// SetCloud turns cloud access on or off in project.toml.
func (p *Project) SetCloud(on bool) error {
	st, err := p.Settings()
	if err != nil {
		return err
	}
	st.Cloud = on
	var b strings.Builder
	b.WriteString("# Project settings (PRD D37). cloud = true lets cloud profiles see this project.\n")
	if err := toml.NewEncoder(&b).Encode(st); err != nil {
		return err
	}
	path := filepath.Join(p.Dir, "project.toml")
	tmp := path + ".partial"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Settings reads project.toml; a missing file means the defaults.
func (p *Project) Settings() (Settings, error) {
	var st Settings
	_, err := toml.DecodeFile(filepath.Join(p.Dir, "project.toml"), &st)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	return st, err
}

// Limits on what goes into every turn's prompt.
const (
	maxInstructions = 6000
	maxMemory       = 6000
)

func readCapped(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		s = s[:n] + "\n[... cut: the file is longer than " + fmt.Sprint(n) + " characters]"
	}
	return s
}

// Context is the text added to the system prompt while the project is
// attached: instructions, memory, the list of sources, and how to use the
// tools.
func (p *Project) Context(ctx context.Context, tools bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are working in the project %q.\n", p.Name)
	if t := readCapped(filepath.Join(p.Dir, "PROJECT.md"), maxInstructions); t != "" {
		b.WriteString("\n## Project instructions\n" + t + "\n")
	}
	if m := readCapped(filepath.Join(p.Dir, "memory.md"), maxMemory); m != "" {
		b.WriteString("\n## Remembered facts\n" + m + "\n")
	}
	srcs, _ := p.Index.Sources(ctx)
	if len(srcs) > 0 {
		b.WriteString("\n## Sources\n")
		for _, s := range srcs {
			switch {
			case s.Error != "" && s.Chunks == 0:
				fmt.Fprintf(&b, "- %s (unreadable: %s)\n", s.Name, s.Error)
			case s.Pages > 1:
				fmt.Fprintf(&b, "- %s (%d pages)\n", s.Name, s.Pages)
			default:
				fmt.Fprintf(&b, "- %s\n", s.Name)
			}
		}
	}
	if tools {
		b.WriteString("\nAnswer questions about this project from its sources: use the passages below, and call project_search for more; " +
			"use project_read to read a whole page. Cite sources like (Owner's Manual.pdf p. 84). " +
			"If the sources don't cover something, say so, then answer from general knowledge and say which part is which. " +
			"Use remember to save a lasting fact the user tells you (it asks them first).\n")
	}
	return b.String()
}

// prefetch is how many passages are found for every message before the
// model is called.
const prefetch = 4

// Passages searches the sources for the message itself and returns the best
// passages as prompt text. They go into every turn (not the saved history),
// because small models often answer without calling project_search even when
// told to (PRD D37).
func (p *Project) Passages(ctx context.Context, message string) string {
	hits, err := p.Index.Search(ctx, message, prefetch)
	if err != nil || len(hits) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n## Passages from the sources that may answer this message\n" +
		"(found automatically; use them if they fit, cite them, and call project_search for more)\n\n")
	for _, h := range hits {
		where := h.Source
		if h.Page > 0 {
			where += fmt.Sprintf(" p. %d", h.Page)
		}
		fmt.Fprintf(&b, "(%s)\n%s\n\n", where, h.Text)
	}
	return b.String()
}

// Remember appends a fact to memory.md.
func (p *Project) Remember(fact string) error {
	fact = strings.TrimSpace(strings.ReplaceAll(fact, "\n", " "))
	if fact == "" {
		return errors.New("nothing to remember")
	}
	if len(fact) > 500 {
		return errors.New("keep a fact under 500 characters")
	}
	p.memMu.Lock()
	defer p.memMu.Unlock()
	path := filepath.Join(p.Dir, "memory.md")
	if b, _ := os.ReadFile(path); len(b)+len(fact) > maxMemory {
		return fmt.Errorf("memory.md is full (%d characters); edit it to make room", maxMemory)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "- %s (%s)\n", fact, time.Now().Format("2006-01-02"))
	return err
}

// AddSource saves a file into sources/. An existing file of the same name is
// replaced.
func (p *Project) AddSource(name string, data []byte) (string, error) {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" || name == "." || strings.HasPrefix(name, ".") {
		return "", errors.New("the file needs a name")
	}
	if Kind(name) == "" {
		return "", fmt.Errorf("%s: only PDF, text (.md, .txt, .csv) and image files can be project sources", name)
	}
	dir := filepath.Join(p.Dir, "sources")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp := filepath.Join(dir, "."+name+".partial")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return "", err
	}
	return name, os.Rename(tmp, filepath.Join(dir, name))
}
