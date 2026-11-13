package sessions

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ahplummer/gostafson/internal/client"
	"github.com/ahplummer/gostafson/internal/media"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sub", "s.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func roles(msgs []client.Message) string {
	out := ""
	for _, m := range msgs {
		out += m.Role[:1]
	}
	return out
}

func TestAppendRecentReset(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)

	turn := func(chat, text string) {
		if err := s.Append(ctx, chat,
			client.Message{Role: "user", Content: text},
			client.Message{Role: "assistant", Content: "re: " + text}); err != nil {
			t.Fatal(err)
		}
	}
	turn("a", "one")
	turn("a", "two")
	turn("b", "other")

	got, err := s.Recent(ctx, "a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if roles(got) != "uaua" || got[0].Content != "one" || got[3].Content != "re: two" {
		t.Errorf("recent = %+v", got)
	}

	// A limit that cuts mid-turn drops the orphaned assistant message.
	got, _ = s.Recent(ctx, "a", 3)
	if roles(got) != "ua" || got[0].Content != "two" {
		t.Errorf("recent(3) = %+v", got)
	}

	if n, err := s.Count(ctx, "a"); err != nil || n != 4 {
		t.Errorf("count = %d, %v", n, err)
	}

	if err := s.Reset(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Recent(ctx, "a", 10)
	if len(got) != 0 {
		t.Errorf("after reset = %+v", got)
	}
	got, _ = s.Recent(ctx, "b", 10)
	if len(got) != 2 {
		t.Errorf("other chat affected: %+v", got)
	}
}

func TestToolMessagesRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	in := []client.Message{
		{Role: "user", Content: "read a.txt"},
		{Role: "assistant", ToolCalls: []client.ToolCall{{ID: "c1", Type: "function",
			Function: client.FunctionCall{Name: "read_file", Arguments: `{"path":"a.txt"}`}}}},
		{Role: "tool", ToolCallID: "c1", Content: "hello"},
		{Role: "assistant", Content: "It says hello."},
	}
	if err := s.Append(ctx, "c", in...); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Recent(ctx, "c", 10)
	if len(got) != 4 || got[1].ToolCalls[0].Function.Name != "read_file" || got[2].ToolCallID != "c1" {
		t.Errorf("round trip = %+v", got)
	}

	// Cutting into the tool sequence drops back to the next user message (none here).
	got, _ = s.Recent(ctx, "c", 2)
	if len(got) != 0 {
		t.Errorf("recent(2) = %+v", got)
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	s, path := open(t)
	s.Append(ctx, "a", client.Message{Role: "user", Content: "x"})
	s.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, _ := s2.Recent(ctx, "a", 10)
	if len(got) != 1 {
		t.Errorf("after reopen = %+v", got)
	}
}

func TestTextOnly(t *testing.T) {
	in := []client.Message{
		{Role: "user", Content: "read a.txt"},
		{Role: "assistant", ToolCalls: []client.ToolCall{{ID: "c1", Function: client.FunctionCall{Name: "read_file", Arguments: `{"path":"a.txt"}`}}}},
		{Role: "tool", ToolCallID: "c1", Content: "SECRET FILE CONTENTS"},
		{Role: "assistant", Content: "Checking.", ToolCalls: []client.ToolCall{{ID: "c2", Function: client.FunctionCall{Name: "write_file", Arguments: `{"path":"b.txt","content":"PRIVATE DRAFT"}`}}}},
		{Role: "tool", ToolCallID: "c2", Content: "MORE CONTENTS"},
		{Role: "assistant", Content: "It's a greeting."},
		{Role: "user", Content: "@claude is that ok?"},
	}
	got := TextOnly(in)
	if roles(got) != "uau" {
		t.Fatalf("roles = %q, want uau: %+v", roles(got), got)
	}
	want := "[read_file a.txt ran on the local machine; its output is not shared with this model]\n" +
		"Checking.\n[write_file b.txt ran on the local machine; its output is not shared with this model]\n" +
		"It's a greeting."
	if got[1].Content != want {
		t.Errorf("assistant = %q\nwant      %q", got[1].Content, want)
	}
	for _, m := range got {
		for _, secret := range []string{"SECRET FILE CONTENTS", "MORE CONTENTS", "PRIVATE DRAFT"} {
			if strings.Contains(m.Content, secret) {
				t.Errorf("leaked %q in %+v", secret, m)
			}
		}
		if m.Role == "tool" || len(m.ToolCalls) > 0 {
			t.Errorf("tool message kept: %+v", m)
		}
	}
	if in[3].ToolCalls == nil || in[3].Content != "Checking." {
		t.Error("TextOnly modified its input")
	}
}

// Image data never reaches the database; the refs do.
func TestImagesStoredAsRefs(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	m := client.Message{Role: "user", Content: "look", ImageRefs: []string{"abc.png"},
		Images: []media.Image{{MediaType: "image/png", Data: []byte("PIXELS")}}}
	if err := s.Append(ctx, "c", m); err != nil {
		t.Fatal(err)
	}
	var raw string
	s.db.QueryRow(`SELECT message FROM messages`).Scan(&raw)
	if strings.Contains(raw, "PIXELS") || strings.Contains(raw, "UElYRUxT") || !strings.Contains(raw, "abc.png") {
		t.Errorf("stored %s", raw)
	}
	got, _ := s.Recent(ctx, "c", 10)
	if len(got) != 1 || len(got[0].ImageRefs) != 1 || got[0].Images != nil {
		t.Errorf("recent = %+v", got)
	}
}

func TestLabelOthers(t *testing.T) {
	msgs := []client.Message{
		{Role: "user", Content: "what color is this?"},
		{Role: "assistant", Content: "Red.", Profile: "default"},
		{Role: "user", Content: "@opus what is it?"},
		{Role: "assistant", Content: "A square.", Profile: "opus"},
		{Role: "assistant", Content: "Old reply.", Profile: ""}, // saved before profiles were recorded
		{Role: "assistant", Content: "", Profile: "code", ToolCalls: []client.ToolCall{{ID: "1"}}},
	}
	got := LabelOthers(msgs, "opus", map[string]string{"default": "gostafson-qwen3.5-9b"})
	want := []string{
		"what color is this?",
		"[reply from @default (gostafson-qwen3.5-9b), a different model, not you]\nRed.",
		"@opus what is it?",
		"A square.",
		"Old reply.",
		"",
	}
	for i, w := range want {
		if got[i].Content != w {
			t.Errorf("message %d = %q, want %q", i, got[i].Content, w)
		}
	}
}

// The profile is stored with history, survives TextOnly, and never goes on
// the wire.
func TestProfileStoredNotSent(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	s.Append(ctx, "c", client.Message{Role: "user", Content: "hi"}, client.Message{Role: "assistant", Content: "hello", Profile: "luna"})
	got, _ := s.Recent(ctx, "c", 10)
	if len(got) != 2 || got[1].Profile != "luna" {
		t.Fatalf("recent = %+v", got)
	}
	if to := TextOnly(got); to[1].Profile != "luna" {
		t.Errorf("TextOnly lost the profile: %+v", to[1])
	}
	wire, _ := json.Marshal(got[1])
	if strings.Contains(string(wire), "luna") {
		t.Errorf("profile on the wire: %s", wire)
	}
}
