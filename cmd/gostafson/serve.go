package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ahplummer/gostafson/internal/transport"
)

// serve runs a receive loop per transport until one of them stops: /exit on
// the CLI, or a transport failure (the service manager restarts the process).
func (a *app) serve(ctx context.Context, trs []transport.Transport) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, len(trs))
	for _, tr := range trs {
		go func() { errc <- a.receiveLoop(ctx, tr) }()
	}
	err := <-errc
	cancel()
	a.wg.Wait()
	return err
}

func (a *app) receiveLoop(ctx context.Context, tr transport.Transport) error {
	_, sequential := tr.(transport.Sequential)
	for {
		msg, err := tr.Receive(ctx)
		if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", tr.Name(), err)
		}
		if sequential {
			a.process(ctx, tr, msg)
			continue
		}
		a.dispatch(ctx, tr, msg)
	}
}

// dispatch hands msg to its chat's worker, starting one if needed. Messages
// in one chat run in order; different chats run in parallel. Chat IDs carry a
// transport prefix, so a worker belongs to one transport.
func (a *app) dispatch(ctx context.Context, tr transport.Transport, msg transport.Message) {
	a.mu.Lock()
	queue, ok := a.workers[msg.ChatID]
	if !ok {
		queue = make(chan transport.Message, 8)
		a.workers[msg.ChatID] = queue
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case m := <-queue:
					a.process(ctx, tr, m)
				}
			}
		}()
	}
	a.mu.Unlock()
	select {
	case queue <- msg:
	default:
		tr.Send(ctx, msg.ChatID, "busy: too many messages waiting in this chat; try again in a moment")
	}
}

// process handles one message and sends the reply on the transport it came from.
func (a *app) process(ctx context.Context, tr transport.Transport, msg transport.Message) {
	done := func() {}
	if ind, ok := tr.(transport.Indicator); ok && !strings.HasPrefix(strings.TrimSpace(msg.Text), "/") {
		done = ind.Busy(msg.ChatID)
	}
	reply := a.handle(ctx, tr, msg)
	done()
	if err := tr.Send(ctx, msg.ChatID, reply); err != nil {
		a.log.Error("send failed", "transport", tr.Name(), "chat", msg.ChatID, "err", err)
	}
}
