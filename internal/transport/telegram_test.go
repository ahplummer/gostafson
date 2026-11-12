package transport

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBot is a minimal Bot API: it serves queued updates once each and
// records every other call.
type fakeBot struct {
	mu      sync.Mutex
	updates []string // JSON update objects
	calls   []string // "method {json}"
}

func (f *fakeBot) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/botTOKEN/") {
			t.Errorf("path %q", r.URL.Path)
		}
		method := strings.TrimPrefix(r.URL.Path, "/botTOKEN/")
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		if method == "getUpdates" {
			ups := "[" + strings.Join(f.updates, ",") + "]"
			f.updates = nil
			io.WriteString(w, `{"ok":true,"result":`+ups+`}`)
			return
		}
		f.calls = append(f.calls, method+" "+string(body))
		io.WriteString(w, `{"ok":true,"result":{}}`)
	})
}

func (f *fakeBot) push(u ...string) { f.mu.Lock(); f.updates = append(f.updates, u...); f.mu.Unlock() }
func (f *fakeBot) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func msgUpdate(id int, from int64, chatType, text string) string {
	b, _ := json.Marshal(map[string]any{"update_id": id, "message": map[string]any{
		"from": map[string]any{"id": from}, "chat": map[string]any{"id": from, "type": chatType}, "text": text}})
	return string(b)
}

func newFakeTelegram(t *testing.T) (*Telegram, *fakeBot) {
	bot := &fakeBot{}
	srv := httptest.NewServer(bot.handler(t))
	t.Cleanup(srv.Close)
	return NewTelegram(srv.URL, "TOKEN", []int64{42}, slog.New(slog.NewTextHandler(io.Discard, nil))), bot
}

func TestTelegramAllowlist(t *testing.T) {
	tg, bot := newFakeTelegram(t)
	bot.push(
		msgUpdate(1, 99, "private", "from a stranger"),
		msgUpdate(2, 42, "group", "owner, but in a group"),
		msgUpdate(3, 42, "private", "@code hello"),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	m, err := tg.Receive(ctx)
	if err != nil || m.Text != "@code hello" || m.ChatID != "tg:42" {
		t.Fatalf("receive = %+v, %v", m, err)
	}
	if tg.offset != 4 {
		t.Errorf("offset = %d", tg.offset)
	}
}

func TestTelegramSendSplits(t *testing.T) {
	tg, bot := newFakeTelegram(t)
	long := strings.Repeat("line of text\n", 700) // ~9,100 chars
	if err := tg.Send(context.Background(), "tg:42", long); err != nil {
		t.Fatal(err)
	}
	calls := bot.callList()
	if len(calls) != 3 {
		t.Fatalf("%d sendMessage calls, want 3", len(calls))
	}
	for _, c := range calls {
		if !strings.HasPrefix(c, "sendMessage ") || !strings.Contains(c, `"parse_mode":"HTML"`) {
			t.Errorf("call %.60q", c)
		}
	}
	if err := tg.Send(context.Background(), "web", "x"); err == nil {
		t.Error("non-telegram chat: expected error")
	}
}

func TestTelegramAskWithButton(t *testing.T) {
	tg, bot := newFakeTelegram(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go tg.Receive(ctx) // polls updates in the background, as main's receive loop does

	answer := make(chan string)
	go func() {
		a, _ := tg.Ask(ctx, "tg:42", "overwrite a.txt (1 -> 2 bytes)? [y/N]")
		answer <- a
	}()
	for !tg.pending.Waiting("tg:42") {
		time.Sleep(5 * time.Millisecond)
	}
	// A button press from someone else is ignored; the owner's counts.
	bot.push(`{"update_id":10,"callback_query":{"id":"cb1","from":{"id":99},"message":{"message_id":5,"chat":{"id":42}},"data":"yes"}}`,
		`{"update_id":11,"callback_query":{"id":"cb2","from":{"id":42},"message":{"message_id":5,"chat":{"id":42}},"data":"no"}}`)
	select {
	case a := <-answer:
		if a != "no" {
			t.Errorf("answer = %q, want the owner's no", a)
		}
	case <-ctx.Done():
		t.Fatal("no answer")
	}
	var joined string
	for i := 0; i < 100 && !strings.Contains(joined, "editMessageReplyMarkup"); i++ {
		time.Sleep(10 * time.Millisecond) // the button edit is sent just after the answer is delivered
		joined = strings.Join(bot.callList(), "\n")
	}
	if !strings.Contains(joined, `"callback_data":"yes"`) || !strings.Contains(joined, "editMessageReplyMarkup") {
		t.Errorf("calls = %s", joined)
	}
}

func TestTelegramErrorsHideToken(t *testing.T) {
	tg := NewTelegram("http://127.0.0.1:1", "123:SECRET", []int64{42}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	err := tg.call(context.Background(), "getMe", map[string]any{}, nil)
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("err = %v", err)
	}
}

func TestSplitText(t *testing.T) {
	parts := splitText(strings.Repeat("é", 25), 10)
	if len(parts) != 3 || len([]rune(parts[0])) != 10 || len([]rune(parts[2])) != 5 {
		t.Errorf("parts = %q", parts)
	}
	if got := splitText("short", 10); len(got) != 1 || got[0] != "short" {
		t.Errorf("short = %q", got)
	}
}

func TestTGHTML(t *testing.T) {
	in := "Because they **make up everything**!\nRun `ls -la` <now> & then:\n```sh\necho \"<b>\"\n```\ndone"
	want := "Because they <b>make up everything</b>!\nRun <code>ls -la</code> &lt;now&gt; &amp; then:\n<pre>echo &#34;&lt;b&gt;&#34;</pre>\ndone"
	if got := tgHTML(in); got != want {
		t.Errorf("tgHTML =\n%s\nwant\n%s", got, want)
	}
	if got := tgHTML("```\nunclosed <x>"); got != "<pre>unclosed &lt;x&gt;</pre>" {
		t.Errorf("unclosed fence = %q", got)
	}
	if got := tgHTML("<script>alert(1)</script>"); strings.Contains(got, "<script>") {
		t.Errorf("tags not escaped: %q", got)
	}
}

func TestTelegramFallsBackToPlain(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		if strings.Contains(string(b), "parse_mode") {
			io.WriteString(w, `{"ok":false,"description":"Bad Request: can't parse entities"}`)
			return
		}
		io.WriteString(w, `{"ok":true,"result":{}}`)
	}))
	defer srv.Close()
	tg := NewTelegram(srv.URL, "TOKEN", []int64{42}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := tg.Send(context.Background(), "tg:42", "**hi**"); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || !strings.Contains(bodies[1], `"text":"**hi**"`) || strings.Contains(bodies[1], "parse_mode") {
		t.Errorf("bodies = %q", bodies)
	}
}
