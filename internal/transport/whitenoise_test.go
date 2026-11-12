package transport

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	owner   = "aa00000000000000000000000000000000000000000000000000000000000001"
	agentID = "bb00000000000000000000000000000000000000000000000000000000000002"
	group   = "cc00000000000000000000000000000000000000000000000000000000000003"
)

// fakeAgent is a minimal wn-agent: account_list, subscribe_inbound (then
// events pushed by the test), and send_final (recorded).
type fakeAgent struct {
	path   string
	mu     sync.Mutex
	sent   []map[string]any
	events chan map[string]any
}

func newFakeAgent(t *testing.T) *fakeAgent {
	dir, err := os.MkdirTemp("", "wn") // short path: Unix socket paths are limited to ~104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	f := &fakeAgent{path: filepath.Join(dir, "s.sock"), events: make(chan map[string]any, 16)}
	ln, err := net.Listen("unix", f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeAgent) serve(c net.Conn) {
	defer c.Close()
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		return
	}
	var req map[string]any
	json.Unmarshal(line, &req)
	reply := func(m map[string]any) {
		m["marmot_agent_control"] = wnProtocol
		m["id"] = req["id"]
		b, _ := json.Marshal(m)
		c.Write(append(b, '\n'))
	}
	switch req["type"] {
	case "account_list":
		reply(map[string]any{"type": "account_list", "accounts": []any{
			map[string]any{"account_id_hex": agentID, "label": "gostafson", "local_signing": true}}})
	case "send_final":
		f.mu.Lock()
		f.sent = append(f.sent, req)
		f.mu.Unlock()
		reply(map[string]any{"type": "final_sent"})
	case "subscribe_inbound":
		reply(map[string]any{"type": "ack"})
		for ev := range f.events {
			reply(ev)
		}
	default:
		reply(map[string]any{"type": "error", "code": "unknown", "message": "unknown request"})
	}
}

func (f *fakeAgent) sentTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, s := range f.sent {
		out = append(out, s["text"].(string))
	}
	return out
}

func inbound(from, text string) map[string]any {
	return map[string]any{"type": "inbound_message", "account_id_hex": agentID, "group_id_hex": group,
		"message": map[string]any{"message_id_hex": "dd", "text": text, "sender": map[string]any{"account_id_hex": from}}}
}

func newTestWN(t *testing.T, f *fakeAgent) *WhiteNoise {
	w, err := NewWhiteNoise(f.path, "", "", []string{owner}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestWhiteNoiseReceiveFiltersSenders(t *testing.T) {
	f := newFakeAgent(t)
	w := newTestWN(t, f)
	f.events <- map[string]any{"type": "reaction_added"}
	f.events <- inbound("ee00000000000000000000000000000000000000000000000000000000000009", "stranger")
	f.events <- inbound(agentID, "my own message")
	f.events <- inbound(owner, "@code hello")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	m, err := w.Receive(ctx)
	if err != nil || m.Text != "@code hello" || m.ChatID != "wn:"+group || m.Sender != owner {
		t.Fatalf("receive = %+v, %v", m, err)
	}
	if w.account != agentID {
		t.Errorf("account = %q", w.account)
	}
}

func TestWhiteNoiseSendAndAsk(t *testing.T) {
	f := newFakeAgent(t)
	w := newTestWN(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.Send(ctx, "wn:"+group, "hi there"); err != nil {
		t.Fatal(err)
	}
	if err := w.Send(ctx, "web", "x"); err == nil {
		t.Error("non-whitenoise chat: expected error")
	}

	go w.Receive(ctx)
	answer := make(chan string)
	go func() {
		a, _ := w.Ask(ctx, "wn:"+group, "overwrite a.txt? [y/N]")
		answer <- a
	}()
	for !w.pending.Waiting("wn:" + group) {
		time.Sleep(5 * time.Millisecond)
	}
	f.events <- inbound(owner, "yes")
	if a := <-answer; a != "yes" {
		t.Errorf("answer = %q", a)
	}
	got := f.sentTexts()
	if len(got) != 2 || got[0] != "hi there" || !strings.Contains(got[1], "overwrite a.txt?") {
		t.Errorf("sent = %q", got)
	}
	f.mu.Lock()
	if f.sent[0]["group_id_hex"] != group || f.sent[0]["account_id_hex"] != agentID || f.sent[0]["idempotency_key"] == "" {
		t.Errorf("send_final = %v", f.sent[0])
	}
	f.mu.Unlock()
}

func TestWhiteNoiseReceiveStopsOnCancel(t *testing.T) {
	f := newFakeAgent(t)
	w := newTestWN(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { _, err := w.Receive(ctx); done <- err }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("expected an error after cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Receive did not return after cancel")
	}
}

func TestPubkeyHex(t *testing.T) {
	// Example from NIP-19.
	got, err := PubkeyHex("npub10elfcs4fr0l0r8af98jlmgdh9c8tcxjvz9qkw038js35mp4dma8qzvjptg")
	if err != nil || got != "7e7e9c42a91bfef19fa929e5fda1b72e0ebc1a4c1141673e2794234d86addf4e" {
		t.Errorf("npub = %q, %v", got, err)
	}
	if got, err := PubkeyHex(strings.ToUpper(owner)); err != nil || got != owner {
		t.Errorf("hex = %q, %v", got, err)
	}
	for _, bad := range []string{"npub10elfcs4fr0l0r8af98jlmgdh9c8tcxjvz9qkw038js35mp4dma8qzvjptx", "nsec1abc", "abc", ""} {
		if _, err := PubkeyHex(bad); err == nil {
			t.Errorf("PubkeyHex(%q): expected error", bad)
		}
	}
	if _, err := NewWhiteNoise("/x", "", "", nil, nil); err == nil {
		t.Error("no allowed senders: expected error")
	}
}
