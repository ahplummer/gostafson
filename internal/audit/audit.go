// Package audit writes one JSON line per turn and per tool call.
//
// Records use OpenTelemetry-style IDs: a turn is a trace with one root span;
// each tool call is a child span. Subagent turns (M7) will reuse the parent's
// trace ID, so a whole turn can be read back, or exported to a tracing UI.
//
// The log contains prompts and tool arguments, so the file is created with
// mode 0600 in a 0700 directory.
package audit

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"
)

// Turn is the record for one user message.
type Turn struct {
	Time             time.Time `json:"time"`
	Kind             string    `json:"kind"` // "turn"
	TraceID          string    `json:"trace_id"`
	SpanID           string    `json:"span_id"`
	Chat             string    `json:"chat"`
	Profile          string    `json:"profile"`
	RoutedBy         string    `json:"routed_by"`               // tag | default | decision (M6)
	DecisionProb     *float64  `json:"decision_prob,omitempty"` // set when routed_by = decision
	Model            string    `json:"model"`
	Think            bool      `json:"think"`
	Steps            int       `json:"steps"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	DurationMS       int64     `json:"duration_ms"`
	Error            string    `json:"error,omitempty"`
}

// ToolCall is the record for one tool call, allowed or denied.
type ToolCall struct {
	Time         time.Time       `json:"time"`
	Kind         string          `json:"kind"` // "tool"
	TraceID      string          `json:"trace_id"`
	SpanID       string          `json:"span_id"`
	ParentSpanID string          `json:"parent_span_id"`
	Chat         string          `json:"chat"`
	Profile      string          `json:"profile"`
	Tool         string          `json:"tool"`
	Args         json.RawMessage `json:"args"`
	Guard        string          `json:"guard"` // allow | deny
	Reason       string          `json:"reason,omitempty"`
	Approval     string          `json:"approval,omitempty"` // approved | denied | timeout | unavailable, when asked
	ResultBytes  int             `json:"result_bytes"`
	DurationMS   int64           `json:"duration_ms"`
	Error        string          `json:"error,omitempty"`
}

// Log is an append-only JSONL file. A nil *Log discards records.
type Log struct {
	mu sync.Mutex
	f  *os.File
}

// Open opens or creates the log at path.
func Open(path string) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("audit dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("audit log: %w", err)
	}
	return &Log{f: f}, nil
}

// Write appends one record as a JSON line.
func (l *Log) Write(rec any) error {
	if l == nil {
		return nil
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err = l.f.Write(append(b, '\n'))
	return err
}

// Close closes the file.
func (l *Log) Close() error {
	if l == nil {
		return nil
	}
	return l.f.Close()
}

// NewTraceID returns a random 16-byte ID in hex, as OpenTelemetry uses.
func NewTraceID() string { return randomHex(16) }

// NewSpanID returns a random 8-byte ID in hex.
func NewSpanID() string { return randomHex(8) }

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b) // never returns an error (crypto/rand, Go 1.24+)
	return hex.EncodeToString(b)
}

// maxArgString is the longest string argument kept verbatim. Longer values
// (file contents passed to write_file, mostly) are shortened so the log stays
// readable and doesn't hold a second copy of every file.
const maxArgString = 200

// Args prepares a tool call's raw JSON arguments for the log: long string
// values are cut to a prefix plus their length. Arguments that aren't a JSON
// object are kept as a (shortened) JSON string.
func Args(raw string) json.RawMessage {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		b, _ := json.Marshal(shorten(raw))
		return b
	}
	for k, v := range m {
		if s, ok := v.(string); ok {
			m[k] = shorten(s)
		}
	}
	b, _ := json.Marshal(m)
	return b
}

func shorten(s string) string {
	if len(s) <= maxArgString {
		return s
	}
	cut := maxArgString
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return fmt.Sprintf("%s... (%d bytes)", s[:cut], len(s))
}
