// Package client is a minimal OpenAI-compatible chat completions client.
// It covers what gostafson needs from Ollama, LM Studio and OpenRouter:
// non-streaming chat, tool definitions and tool calls, and reasoning control.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ahplummer/gostafson/internal/media"
)

// Message is one chat message in OpenAI wire format.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`

	// Images go with a user message as image_url content parts. They are
	// never stored; ImageRefs names them in the media store, so history
	// records which images a message had (PRD D34).
	Images    []media.Image `json:"-"`
	ImageRefs []string      `json:"image_refs,omitempty"`
}

// MarshalJSON writes the wire form: content is a string, or a list of text
// and image_url parts when the message has images. ImageRefs is left out.
func (m Message) MarshalJSON() ([]byte, error) {
	type wire struct {
		Role       string     `json:"role"`
		Content    any        `json:"content"`
		ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
		ToolCallID string     `json:"tool_call_id,omitempty"`
	}
	w := wire{Role: m.Role, Content: m.Content, ToolCalls: m.ToolCalls, ToolCallID: m.ToolCallID}
	if len(m.Images) > 0 {
		parts := []map[string]any{}
		if m.Content != "" {
			parts = append(parts, map[string]any{"type": "text", "text": m.Content})
		}
		for _, im := range m.Images {
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{"url": im.DataURL()}})
		}
		w.Content = parts
	}
	return json.Marshal(w)
}

// ToolCall is a model's request to run a tool.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall holds the tool name and its JSON-encoded arguments.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool is a tool definition sent to the model.
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction describes a tool's name, purpose and JSON schema.
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Request is a chat completions request.
type Request struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Tools       []Tool    `json:"tools,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	// ReasoningEffort controls thinking. Ollama accepts "none" to turn it off.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// Provider is OpenRouter's provider routing object, e.g.
	// {"data_collection": "deny"}. Other servers ignore it.
	Provider map[string]any `json:"provider,omitempty"`
	// Plugins is OpenRouter's plugins array, e.g. [{"id": "web"}] for web
	// search. Other servers ignore it.
	Plugins []map[string]any `json:"plugins,omitempty"`
	Stream  bool             `json:"stream"`
}

// Usage is the token accounting returned by the server.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Reply is the first choice of a chat completions response.
type Reply struct {
	Message      Message
	Reasoning    string
	FinishReason string
	Usage        Usage
}

type response struct {
	Choices []struct {
		Message struct {
			Message
			Reasoning        string `json:"reasoning"`         // Ollama, OpenRouter
			ReasoningContent string `json:"reasoning_content"` // some other servers
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage Usage `json:"usage"`
}

// Client talks to one OpenAI-compatible endpoint.
type Client struct {
	BaseURL string // e.g. http://localhost:11434/v1
	APIKey  string // empty for local servers
	HTTP    *http.Client
}

// New returns a client for baseURL. apiKey may be empty.
func New(baseURL, apiKey string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: 10 * time.Minute}, // per-turn deadline comes from ctx
	}
}

// Chat sends one non-streaming chat completions request.
func (c *Client) Chat(ctx context.Context, req Request) (Reply, error) {
	req.Stream = false
	body, err := json.Marshal(req)
	if err != nil {
		return Reply{}, fmt.Errorf("encode request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Reply{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return Reply{}, fmt.Errorf("chat request to %s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return Reply{}, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return Reply{}, fmt.Errorf("chat request to %s: %s: %s", c.BaseURL, resp.Status, truncate(string(data), 500))
	}

	var r response
	if err := json.Unmarshal(data, &r); err != nil {
		return Reply{}, fmt.Errorf("decode response: %w", err)
	}
	if len(r.Choices) == 0 {
		return Reply{}, errors.New("response has no choices")
	}
	ch := r.Choices[0]
	reasoning := ch.Message.Reasoning
	if reasoning == "" {
		reasoning = ch.Message.ReasoningContent
	}
	return Reply{
		Message:      ch.Message.Message,
		Reasoning:    reasoning,
		FinishReason: ch.FinishReason,
		Usage:        r.Usage,
	}, nil
}

// Ping checks that the server answers GET /models.
func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/models", nil)
	if err != nil {
		return err
	}
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return errors.New(resp.Status)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
