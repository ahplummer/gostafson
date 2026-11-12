//go:build integration

package client

// Run with the tunnel up: make tunnel (one terminal), make test-integration (another).
// GOSTAFSON_TEST_BASE_URL and GOSTAFSON_TEST_MODEL override the defaults.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func TestIntegrationThinkingOff(t *testing.T) {
	c := New(env("GOSTAFSON_TEST_BASE_URL", "http://localhost:11434/v1"), "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	reply, err := c.Chat(ctx, Request{
		Model:           env("GOSTAFSON_TEST_MODEL", "qwen3.5:9b"),
		Messages:        []Message{{Role: "user", Content: "What is 17*23? Answer with the number only."}},
		ReasoningEffort: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply.Message.Content, "391") {
		t.Errorf("content = %q", reply.Message.Content)
	}
	if reply.Reasoning != "" {
		t.Errorf("expected no reasoning with reasoning_effort=none, got %d chars", len(reply.Reasoning))
	}
}
