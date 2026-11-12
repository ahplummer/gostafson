package loop

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ahplummer/gostafson/internal/client"
	"github.com/ahplummer/gostafson/internal/config"
	"github.com/ahplummer/gostafson/internal/guard"
)

// fakeChat returns scripted replies in order and records requests.
type fakeChat struct {
	replies []client.Reply
	reqs    []client.Request
}

func (f *fakeChat) Chat(_ context.Context, req client.Request) (client.Reply, error) {
	// Copy messages: the loop reuses its slice.
	req.Messages = append([]client.Message(nil), req.Messages...)
	f.reqs = append(f.reqs, req)
	if len(f.replies) == 0 {
		return client.Reply{}, errors.New("no more replies")
	}
	r := f.replies[0]
	f.replies = f.replies[1:]
	return r, nil
}

type echoTools struct{ calls []string }

func (e *echoTools) Definitions() []client.Tool {
	return []client.Tool{{Type: "function", Function: client.ToolFunction{Name: "echo", Parameters: json.RawMessage(`{}`)}}}
}

func (e *echoTools) Call(_ context.Context, name, args string) (string, error) {
	e.calls = append(e.calls, name+args)
	if name == "fail" {
		return "", errors.New("boom")
	}
	return "echoed " + args, nil
}

func text(s string) client.Reply {
	return client.Reply{Message: client.Message{Role: "assistant", Content: s}, Usage: client.Usage{TotalTokens: 10}}
}

func toolCall(name, args string) client.Reply {
	return client.Reply{Message: client.Message{Role: "assistant", ToolCalls: []client.ToolCall{
		{ID: "id-" + name, Type: "function", Function: client.FunctionCall{Name: name, Arguments: args}}}},
		Usage: client.Usage{TotalTokens: 5}}
}

func profile(toggle bool) *config.Profile {
	return &config.Profile{Name: "default", Model: "m", SystemPrompt: "sys", ThinkToggle: toggle}
}

func TestPlainReply(t *testing.T) {
	f := &fakeChat{replies: []client.Reply{text("hi")}}
	hist := []client.Message{{Role: "user", Content: "earlier"}, {Role: "assistant", Content: "ok"}}
	res, err := Run(context.Background(), f, nil, Turn{Profile: profile(true), History: hist, Input: "hello", MaxSteps: 8})
	if err != nil {
		t.Fatal(err)
	}
	if res.Reply != "hi" || res.Steps != 1 || len(res.New) != 2 {
		t.Errorf("result = %+v", res)
	}
	req := f.reqs[0]
	if req.ReasoningEffort != "none" {
		t.Errorf("think toggle off should send reasoning_effort=none, got %q", req.ReasoningEffort)
	}
	if len(req.Messages) != 4 || req.Messages[0].Role != "system" || req.Messages[3].Content != "hello" {
		t.Errorf("messages = %+v", req.Messages)
	}
	if req.Tools != nil {
		t.Error("no tools expected")
	}
}

func TestThinkFlag(t *testing.T) {
	for _, tc := range []struct {
		toggle, think bool
		want          string
	}{
		{true, false, "none"},
		{true, true, ""},
		{false, false, ""}, // profiles without the toggle never send it (e.g. cloud models)
	} {
		f := &fakeChat{replies: []client.Reply{text("x")}}
		Run(context.Background(), f, nil, Turn{Profile: profile(tc.toggle), Think: tc.think, Input: "q", MaxSteps: 1})
		if got := f.reqs[0].ReasoningEffort; got != tc.want {
			t.Errorf("toggle=%v think=%v: reasoning_effort = %q, want %q", tc.toggle, tc.think, got, tc.want)
		}
	}
}

func TestToolRoundTrip(t *testing.T) {
	f := &fakeChat{replies: []client.Reply{toolCall("echo", `{"a":1}`), toolCall("fail", `{}`), text("done")}}
	tools := &echoTools{}
	res, err := Run(context.Background(), f, tools, Turn{Profile: profile(false), Input: "go", MaxSteps: 8})
	if err != nil {
		t.Fatal(err)
	}
	if res.Reply != "done" || res.Steps != 3 || res.Usage.TotalTokens != 20 {
		t.Errorf("result = %+v", res)
	}
	// user, assistant(call), tool, assistant(call), tool, assistant(text)
	if len(res.New) != 6 || res.New[2].Content != `echoed {"a":1}` || res.New[2].ToolCallID != "id-echo" {
		t.Errorf("new = %+v", res.New)
	}
	if res.New[4].Content != "error: boom" {
		t.Errorf("tool error not fed back: %+v", res.New[4])
	}
	if len(f.reqs[2].Messages) != 6 { // system, user, then two (assistant call, tool result) pairs
		t.Errorf("third request has %d messages", len(f.reqs[2].Messages))
	}
}

func TestStepLimit(t *testing.T) {
	f := &fakeChat{replies: []client.Reply{toolCall("echo", "1"), toolCall("echo", "2"), toolCall("echo", "3")}}
	res, err := Run(context.Background(), f, &echoTools{}, Turn{Profile: profile(false), Input: "go", MaxSteps: 2})
	if !errors.Is(err, ErrStepLimit) || res.Steps != 2 {
		t.Errorf("err = %v, steps = %d", err, res.Steps)
	}
}

func TestToolCallWithoutTools(t *testing.T) {
	f := &fakeChat{replies: []client.Reply{toolCall("echo", "{}")}}
	_, err := Run(context.Background(), f, nil, Turn{Profile: profile(false), Input: "go", MaxSteps: 8})
	if err == nil || !strings.Contains(err.Error(), "has no tools") {
		t.Errorf("err = %v", err)
	}
}

// M3: a cloud profile's requests carry its provider preferences and no tool
// definitions, because its guard allows no tools.
func TestCloudRequestHasNoToolsAndCarriesProvider(t *testing.T) {
	g, err := guard.New("cloud", nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeChat{replies: []client.Reply{text("hi")}}
	p := &config.Profile{Name: "cloud", Model: "m", Provider: map[string]any{"data_collection": "deny"},
		Plugins: []map[string]any{{"id": "web", "engine": "exa", "max_results": 5}}}
	if _, err := Run(context.Background(), f, g.ForTurn("c", "t", "s", nil), Turn{Profile: p, Input: "hello", MaxSteps: 8}); err != nil {
		t.Fatal(err)
	}
	req := f.reqs[0]
	if len(req.Tools) != 0 {
		t.Errorf("tools sent to cloud profile: %+v", req.Tools)
	}
	if req.Provider["data_collection"] != "deny" {
		t.Errorf("provider = %v", req.Provider)
	}
	body, _ := json.Marshal(req)
	if strings.Contains(string(body), `"tools"`) || !strings.Contains(string(body), `"provider":{"data_collection":"deny"}`) ||
		!strings.Contains(string(body), `"plugins":[{"engine":"exa","id":"web","max_results":5}]`) {
		t.Errorf("request body = %s", body)
	}
}
