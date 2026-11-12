// Package transport defines how messages reach gostafson and how replies go
// back. Each channel (CLI, web page, messenger) implements Transport.
package transport

import (
	"context"
	"sync"
)

// Message is one incoming message.
type Message struct {
	ChatID string // conversation key, prefixed by transport ("web:", "tg:"); history is stored per chat
	Sender string
	Text   string
}

// Transport receives messages and sends replies.
type Transport interface {
	// Name identifies the transport in logs ("cli", "web", "telegram").
	Name() string
	// Receive blocks until the next message arrives. It returns io.EOF when
	// the channel is closed.
	Receive(ctx context.Context) (Message, error)
	// Send delivers text to a chat.
	Send(ctx context.Context, chatID, text string) error
}

// Optional capabilities. main checks for them with type assertions.
type (
	// Asker asks a yes/no question in a chat and waits for the reply
	// (approve.Asker).
	Asker interface {
		Ask(ctx context.Context, chatID, question string) (string, error)
	}
	// Indicator shows that a turn is running ("typing", a spinner) until
	// done is called.
	Indicator interface {
		Busy(chatID string) (done func())
	}
	// Sequential transports want each message handled before Receive is
	// called again (the CLI: the next line may be an approval answer).
	Sequential interface {
		Sequential()
	}
)

// Footered transports say whether replies end with the
// "[profile | model | time | tokens]" status line. Without the method, they do.
type Footered interface {
	Footer() bool
}

// ShowFooter reports whether replies on tr get the status line.
func ShowFooter(tr Transport) bool {
	f, ok := tr.(Footered)
	return !ok || f.Footer()
}

// Pending routes a chat's next message to a waiting question instead of
// treating it as a new message. Transports that receive messages
// asynchronously (web, messengers) use it to implement Asker.
type Pending struct {
	mu      sync.Mutex
	waiting map[string]chan string
}

// Wait registers a question for chatID and blocks until Deliver gets that
// chat's next message or ctx ends.
func (p *Pending) Wait(ctx context.Context, chatID string) (string, error) {
	ch := make(chan string, 1)
	p.mu.Lock()
	if p.waiting == nil {
		p.waiting = map[string]chan string{}
	}
	p.waiting[chatID] = ch
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		if p.waiting[chatID] == ch {
			delete(p.waiting, chatID)
		}
		p.mu.Unlock()
	}()
	select {
	case ans := <-ch:
		return ans, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Deliver hands text to a question waiting in chatID and reports whether
// there was one. A false result means text is an ordinary message.
func (p *Pending) Deliver(chatID, text string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	ch, ok := p.waiting[chatID]
	if !ok {
		return false
	}
	delete(p.waiting, chatID)
	ch <- text
	return true
}

// Waiting reports whether a question is open in chatID.
func (p *Pending) Waiting(chatID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.waiting[chatID]
	return ok
}
