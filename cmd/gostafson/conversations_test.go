package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ahplummer/gostafson/internal/client"
	"github.com/ahplummer/gostafson/internal/sessions"
)

// A turn that used tools: the title must come from the question and the
// final reply, not from the tool messages at the end.
func TestFirstExchange(t *testing.T) {
	msgs := []client.Message{
		{Role: "user", Content: "what are the winterizing settings?"},
		{Role: "assistant", Content: "Searching.", ToolCalls: []client.ToolCall{{ID: "1"}}},
		{Role: "tool", Content: "manual text", ToolCallID: "1"},
		{Role: "assistant", ToolCalls: []client.ToolCall{{ID: "2"}}},
		{Role: "tool", Content: "more text", ToolCallID: "2"},
		{Role: "assistant", Content: "Turn the valve to Winterize."},
		{Role: "user", Content: "and after?"},
		{Role: "assistant", Content: "Drain it."},
	}
	user, reply := firstExchange(msgs)
	if user != "what are the winterizing settings?" || reply != "Turn the valve to Winterize." {
		t.Errorf("got %q / %q", user, reply)
	}
	if u, _ := firstExchange(msgs[1:6]); u != "" {
		t.Errorf("no user message, got %q", u)
	}
}

// Moving a conversation that searched its project's sources asks first;
// its tool calls are counted from the stored history.
func TestProjectToolResults(t *testing.T) {
	store, err := sessions.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	c, _ := store.New(ctx, "web")
	call := func(name string) client.ToolCall {
		return client.ToolCall{ID: name, Type: "function", Function: client.FunctionCall{Name: name, Arguments: "{}"}}
	}
	store.Append(ctx, c.ID,
		client.Message{Role: "user", Content: "oil?"},
		client.Message{Role: "assistant", ToolCalls: []client.ToolCall{call("project_search"), call("list_dir")}},
		client.Message{Role: "tool", ToolCallID: "project_search", Content: "manual p. 84"},
		client.Message{Role: "tool", ToolCallID: "list_dir", Content: "a.txt"},
		client.Message{Role: "assistant", ToolCalls: []client.ToolCall{call("project_read")}},
		client.Message{Role: "tool", ToolCallID: "project_read", Content: "page text"},
		client.Message{Role: "assistant", Content: "5W-30"})
	a := &app{store: store}
	if n := a.projectToolResults(ctx, c.ID); n != 2 {
		t.Errorf("project tool results = %d, want 2", n)
	}
}
