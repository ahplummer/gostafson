package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteLinesAndMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "audit.jsonl")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	trace := NewTraceID()
	l.Write(Turn{Kind: "turn", TraceID: trace, SpanID: NewSpanID(), Profile: "code"})
	l.Write(ToolCall{Kind: "tool", TraceID: trace, Tool: "read_file", Args: Args(`{"path":"a.txt"}`), Guard: "allow"})
	l.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
	if dir, _ := os.Stat(filepath.Dir(path)); dir.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v, want 0700", dir.Mode().Perm())
	}

	f, _ := os.Open(path)
	defer f.Close()
	var kinds []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		if rec["trace_id"] != trace {
			t.Errorf("trace_id = %v", rec["trace_id"])
		}
		kinds = append(kinds, rec["kind"].(string))
	}
	if strings.Join(kinds, ",") != "turn,tool" {
		t.Errorf("kinds = %v", kinds)
	}
}

func TestIDs(t *testing.T) {
	if a, b := NewTraceID(), NewTraceID(); len(a) != 32 || a == b {
		t.Errorf("trace ids %q %q", a, b)
	}
	if s := NewSpanID(); len(s) != 16 {
		t.Errorf("span id %q", s)
	}
}

func TestArgsShortensLongStrings(t *testing.T) {
	long := strings.Repeat("x", 5000)
	got := string(Args(`{"path":"a.txt","content":"` + long + `"}`))
	if !strings.Contains(got, `"path":"a.txt"`) || !strings.Contains(got, "(5000 bytes)") || len(got) > 400 {
		t.Errorf("args = %s", got)
	}
	if got := string(Args(`not json`)); got != `"not json"` {
		t.Errorf("non-JSON args = %s", got)
	}
}

func TestNilLogDiscards(t *testing.T) {
	var l *Log
	if err := l.Write(Turn{}); err != nil {
		t.Error(err)
	}
}
