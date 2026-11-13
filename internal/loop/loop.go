// Package loop runs one agent turn: call the model, run any tools it asks
// for, feed the results back, and repeat until it answers in plain text or
// hits the step limit.
package loop

import (
	"context"
	"errors"
	"fmt"

	"github.com/ahplummer/gostafson/internal/client"
	"github.com/ahplummer/gostafson/internal/config"
	"github.com/ahplummer/gostafson/internal/media"
)

// Chatter sends one chat request. *client.Client implements it.
type Chatter interface {
	Chat(ctx context.Context, req client.Request) (client.Reply, error)
}

// Tools is the set of tools available for a turn. Nil means no tools.
type Tools interface {
	Definitions() []client.Tool
	Call(ctx context.Context, name, arguments string) (string, error)
}

// Turn is the input to one run of the loop.
type Turn struct {
	Profile   *config.Profile
	Think     bool
	History   []client.Message // earlier messages in this chat, oldest first
	Input     string           // the user's message
	Images    []media.Image    // images sent with it
	ImageRefs []string         // their names in the media store, saved in history
	MaxSteps  int
}

// Result is the outcome of one turn.
type Result struct {
	New       []client.Message // messages added this turn (user, assistant, tool), for the session store
	Reply     string           // final assistant text
	Reasoning string           // reasoning text from the final reply, if any
	Steps     int              // model calls made
	Usage     client.Usage     // summed over all model calls
}

// ErrStepLimit means the model was still calling tools when the limit was reached.
var ErrStepLimit = errors.New("step limit reached")

// Run executes one turn.
func Run(ctx context.Context, c Chatter, tools Tools, t Turn) (Result, error) {
	if t.MaxSteps <= 0 {
		t.MaxSteps = 1
	}
	user := client.Message{Role: "user", Content: t.Input, Images: t.Images, ImageRefs: t.ImageRefs}
	res := Result{New: []client.Message{user}}

	msgs := make([]client.Message, 0, len(t.History)+2)
	if t.Profile.SystemPrompt != "" {
		msgs = append(msgs, client.Message{Role: "system", Content: t.Profile.SystemPrompt})
	}
	msgs = append(msgs, t.History...)
	msgs = append(msgs, user)

	req := client.Request{
		Model:       t.Profile.Model,
		Temperature: t.Profile.Temperature,
		MaxTokens:   t.Profile.MaxTokens,
		Provider:    t.Profile.Provider,
		Plugins:     t.Profile.Plugins,
	}
	if t.Profile.ThinkToggle && !t.Think {
		req.ReasoningEffort = "none"
	}
	if tools != nil {
		req.Tools = tools.Definitions()
	}

	for res.Steps < t.MaxSteps {
		req.Messages = msgs
		reply, err := c.Chat(ctx, req)
		res.Steps++
		if err != nil {
			return res, err
		}
		res.Usage.PromptTokens += reply.Usage.PromptTokens
		res.Usage.CompletionTokens += reply.Usage.CompletionTokens
		res.Usage.TotalTokens += reply.Usage.TotalTokens

		msg := reply.Message
		msg.Role = "assistant"
		msg.Profile = t.Profile.Name
		msgs = append(msgs, msg)
		res.New = append(res.New, msg)

		if len(msg.ToolCalls) == 0 {
			res.Reply = msg.Content
			res.Reasoning = reply.Reasoning
			return res, nil
		}
		if tools == nil {
			return res, fmt.Errorf("model asked for tool %q but this profile has no tools", msg.ToolCalls[0].Function.Name)
		}
		for _, call := range msg.ToolCalls {
			out, err := tools.Call(ctx, call.Function.Name, call.Function.Arguments)
			if err != nil {
				// Tool errors go back to the model so it can correct itself.
				out = "error: " + err.Error()
			}
			tm := client.Message{Role: "tool", ToolCallID: call.ID, Content: out}
			msgs = append(msgs, tm)
			res.New = append(res.New, tm)
		}
	}
	return res, fmt.Errorf("%w (%d model calls)", ErrStepLimit, t.MaxSteps)
}
