package main

import (
	"testing"

	"github.com/ahplummer/gostafson/internal/client"
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
