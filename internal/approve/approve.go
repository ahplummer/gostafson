// Package approve asks the owner a yes/no question over the transport before
// a risky action. Anything other than a clear yes is a no, including no answer
// within the timeout.
package approve

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Asker sends a question to a chat and waits for the reply text.
// transport.CLI implements it.
type Asker interface {
	Ask(ctx context.Context, chatID, question string) (string, error)
}

// Result is the outcome of one approval request, as recorded in the audit log.
type Result string

const (
	Approved    Result = "approved"
	Denied      Result = "denied"
	Timeout     Result = "timeout"
	Unavailable Result = "unavailable" // the transport can't ask
)

// Gate asks questions with a timeout. A nil *Gate answers Unavailable.
type Gate struct {
	asker   Asker
	timeout time.Duration
}

// New returns a gate that asks through asker. A nil asker makes every
// request Unavailable, which callers treat as no.
func New(asker Asker, timeout time.Duration) *Gate {
	return &Gate{asker: asker, timeout: timeout}
}

// Approve asks the question and waits up to the timeout.
func (g *Gate) Approve(ctx context.Context, chatID, question string) Result {
	if g == nil || g.asker == nil {
		return Unavailable
	}
	askCtx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	answer, err := g.asker.Ask(askCtx, chatID, question+" [y/N]")
	if err != nil {
		if errors.Is(askCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return Timeout
		}
		return Denied
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return Approved
	}
	return Denied
}
