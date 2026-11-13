package projects

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/ahplummer/gostafson/internal/client"
	"github.com/ahplummer/gostafson/internal/tools"
)

// Tools returns the project's tools for a turn: project_search and
// project_read (read-only), and remember unless readOnly (a cloud profile
// gets only the two read-only ones).
func (p *Project) Tools(readOnly bool) map[string]tools.Tool {
	t := map[string]tools.Tool{
		"project_search": {Def: def("project_search",
			"Search this project's sources (manuals, notes, screenshots) for passages about a question. Returns the best passages with source and page. Search before answering project questions; try other words if nothing fits.",
			`{"type":"object","properties":{"query":{"type":"string","description":"What to look for, in your own words or the source's (e.g. \"shore power 30 amp adapter\")."}},"required":["query"]}`),
			Run: p.searchTool},
		"project_read": {Def: def("project_read",
			"Read a whole page of a project source, e.g. after a search hit, to see its full context. Page 0 for text files and images.",
			`{"type":"object","properties":{"source":{"type":"string","description":"Source name as listed (e.g. \"Owner's Manual.pdf\")."},"page":{"type":"integer","description":"Page number (from 1; 0 for text files and images)."}},"required":["source"]}`),
			Run: p.readTool},
	}
	if !readOnly {
		t["remember"] = tools.Tool{Def: def("remember",
			"Save a lasting fact about this project to its memory (for example the model year, a part number, a preference). The user is asked first. Not for passing remarks.",
			`{"type":"object","properties":{"fact":{"type":"string","description":"One fact in one sentence."}},"required":["fact"]}`),
			Run: p.rememberTool, Confirm: p.confirmRemember}
	}
	return t
}

func def(name, desc, schema string) client.Tool {
	return client.Tool{Type: "function", Function: client.ToolFunction{Name: name, Description: desc, Parameters: json.RawMessage(schema)}}
}

func (p *Project) searchTool(ctx context.Context, _ *os.Root, raw json.RawMessage) (string, error) {
	var a struct{ Query string }
	if err := json.Unmarshal(raw, &a); err != nil || strings.TrimSpace(a.Query) == "" {
		return "", fmt.Errorf("give a query")
	}
	hits, err := p.Index.Search(ctx, a.Query, 6)
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return "no passages found; try other words, or answer from general knowledge and say so", nil
	}
	var b strings.Builder
	for i, h := range hits {
		where := h.Source
		if h.Page > 0 {
			where += fmt.Sprintf(" p. %d", h.Page)
		}
		fmt.Fprintf(&b, "%d. (%s)\n%s\n\n", i+1, where, h.Text)
		if b.Len() > tools.MaxOutput {
			break
		}
	}
	return cut(b.String()), nil
}

func (p *Project) readTool(ctx context.Context, _ *os.Root, raw json.RawMessage) (string, error) {
	var a struct {
		Source string
		Page   int
	}
	if err := json.Unmarshal(raw, &a); err != nil || a.Source == "" {
		return "", fmt.Errorf("give a source name")
	}
	text, pages, err := p.Index.ReadPage(ctx, a.Source, a.Page)
	if err != nil && a.Page == 0 && pages > 0 { // asked a PDF for "page 0": start at 1
		text, pages, err = p.Index.ReadPage(ctx, a.Source, 1)
	}
	if err != nil {
		return "", err
	}
	head := a.Source
	if pages > 1 {
		head += fmt.Sprintf(", page %d of %d", max(a.Page, 1), pages)
	}
	return cut(head + "\n\n" + strings.TrimSpace(collapse(text))), nil
}

func (p *Project) rememberTool(_ context.Context, _ *os.Root, raw json.RawMessage) (string, error) {
	var a struct{ Fact string }
	json.Unmarshal(raw, &a)
	if err := p.Remember(a.Fact); err != nil {
		return "", err
	}
	return "saved to " + p.Name + "'s memory", nil
}

func (p *Project) confirmRemember(_ *os.Root, raw json.RawMessage) string {
	var a struct{ Fact string }
	json.Unmarshal(raw, &a)
	return fmt.Sprintf("remember in %s: %q?", p.Name, strings.TrimSpace(a.Fact))
}

func cut(s string) string {
	if len(s) <= tools.MaxOutput {
		return s
	}
	return s[:tools.MaxOutput] + "\n[cut]"
}
