package transport

import (
	"bufio"
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func webServer(t *testing.T) (*Web, *httptest.Server, *http.Client) {
	t.Helper()
	w := NewWeb("127.0.0.1:0", "s3cret-token")
	srv := httptest.NewServer(w.Handler())
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	return w, srv, &http.Client{Jar: jar, Timeout: 5 * time.Second}
}

func login(t *testing.T, srv *httptest.Server, c *http.Client, token string) {
	t.Helper()
	resp, err := c.PostForm(srv.URL+"/login", url.Values{"token": {token}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func post(t *testing.T, c *http.Client, u, body string, header bool) int {
	t.Helper()
	req, _ := http.NewRequest("POST", u, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if header {
		req.Header.Set("X-Gostafson", "1")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestWebAuth(t *testing.T) {
	_, srv, c := webServer(t)

	// Not logged in: the page is the login form, the API refuses.
	resp, _ := c.Get(srv.URL + "/")
	body := readAll(resp)
	if !strings.Contains(body, `action="/login"`) {
		t.Errorf("expected login page, got %.80q", body)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("CSP = %q", csp)
	}
	if code := post(t, c, srv.URL+"/api/send", `{"text":"hi"}`, true); code != http.StatusUnauthorized {
		t.Errorf("send without login = %d", code)
	}
	if resp, _ := c.Get(srv.URL + "/api/events"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("events without login = %d", resp.StatusCode)
	}

	login(t, srv, c, "wrong")
	if code := post(t, c, srv.URL+"/api/send", `{"text":"hi"}`, true); code != http.StatusUnauthorized {
		t.Errorf("send after bad login = %d", code)
	}

	login(t, srv, c, "s3cret-token")
	resp, _ = c.Get(srv.URL + "/")
	if body := readAll(resp); !strings.Contains(body, `id="compose"`) {
		t.Errorf("expected chat page after login")
	}
	u, _ := url.Parse(srv.URL)
	for _, ck := range c.Jar.Cookies(u) {
		if strings.Contains(ck.Value, "s3cret") {
			t.Error("cookie contains the raw token")
		}
	}
	// POST without the custom header is refused (CSRF guard).
	if code := post(t, c, srv.URL+"/api/send", `{"text":"hi"}`, false); code != http.StatusForbidden {
		t.Errorf("send without header = %d", code)
	}
}

func TestWebMessagesAndApproval(t *testing.T) {
	w, srv, c := webServer(t)
	login(t, srv, c, "s3cret-token")
	ctx := context.Background()

	if code := post(t, c, srv.URL+"/api/send", `{"text":"@code hello"}`, true); code != http.StatusNoContent {
		t.Fatalf("send = %d", code)
	}
	m, err := w.Receive(ctx)
	if err != nil || m.Text != "@code hello" || m.ChatID != WebChat {
		t.Fatalf("receive = %+v, %v", m, err)
	}

	// While a question is open, the next message answers it instead of
	// becoming a new message.
	answer := make(chan string)
	go func() {
		a, _ := w.Ask(ctx, WebChat, "overwrite a.txt? [y/N]")
		answer <- a
	}()
	for !w.pending.Waiting(WebChat) {
		time.Sleep(time.Millisecond)
	}
	post(t, c, srv.URL+"/api/send", `{"text":"yes"}`, true)
	if a := <-answer; a != "yes" {
		t.Errorf("answer = %q", a)
	}
	select {
	case m := <-w.in:
		t.Errorf("answer also queued as a message: %+v", m)
	default:
	}

	done := w.Busy(WebChat)
	done()
	w.Send(ctx, WebChat, "hi there\n[code | m | 1.0s | 3 tok]")

	// A page that connects now.
	resp, err := c.Get(srv.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var types []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		for _, typ := range []string{"user", "ask", "answered", "busy", "idle", "reply", "ready"} {
			if strings.Contains(line, `"type":"`+typ+`"`) {
				types = append(types, typ)
			}
		}
		if strings.Contains(line, `"type":"ready"`) {
			break
		}
	}
	// Messages come from the saved conversation (/api/history); a page that
	// connects gets only what is still pending: nothing, here.
	if got := strings.Join(types, ","); got != "ready" {
		t.Errorf("events = %s", got)
	}
}

func TestWebAskTimeout(t *testing.T) {
	w, _, _ := webServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := w.Ask(ctx, WebChat, "q?"); err == nil {
		t.Error("expected timeout")
	}
	if w.history[len(w.history)-1].Type != "answered" {
		t.Errorf("last event = %+v", w.history[len(w.history)-1])
	}
}

func readAll(resp *http.Response) string {
	defer resp.Body.Close()
	var b strings.Builder
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		b.WriteString(sc.Text() + "\n")
	}
	return b.String()
}

// An address that isn't on any interface yet (Tailscale at boot) is retried in
// the background instead of failing startup.
func TestWebStartRetriesMissingAddress(t *testing.T) {
	retryInterval = 10 * time.Millisecond
	defer func() { retryInterval = 5 * time.Second }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := NewWeb("192.0.2.1:8484", "t") // TEST-NET-1: never assigned here
	if err := w.Start(ctx, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("Start = %v, want nil (retry in background)", err)
	}
	time.Sleep(50 * time.Millisecond) // a few retries; none may report an error
	select {
	case err := <-w.listenErr:
		t.Fatalf("retry reported %v", err)
	default:
	}
}

// Other listen errors, such as a port already in use, still fail startup.
func TestWebStartFailsOnPortInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	w := NewWeb(ln.Addr().String(), "t")
	if err := w.Start(context.Background(), slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("Start on a port in use = nil, want an error")
	}
}

// An image posted from the page arrives with the message; the history event
// records how many images there were, not their data.
func TestWebImage(t *testing.T) {
	w, srv, c := webServer(t)
	login(t, srv, c, "s3cret-token")
	if code := post(t, c, srv.URL+"/api/send", `{"text":"what is this?","images":[{"name":"shot.png","data":"iVBORw0KGgo="}]}`, true); code != http.StatusNoContent {
		t.Fatalf("send = %d", code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	m, err := w.Receive(ctx)
	if err != nil || len(m.Images) != 1 || m.Images[0].Name != "shot.png" || string(m.Images[0].Data[:4]) != "\x89PNG" {
		t.Fatalf("receive = %+v, %v", m, err)
	}
	last := w.history[len(w.history)-1]
	if last.Type != "user" || last.Images != 1 {
		t.Errorf("event = %+v", last)
	}
	// An image alone is a message too; bad base64 is refused.
	if code := post(t, c, srv.URL+"/api/send", `{"images":[{"name":"a.png","data":"iVBORw0KGgo="}]}`, true); code != http.StatusNoContent {
		t.Errorf("image only = %d", code)
	}
	if code := post(t, c, srv.URL+"/api/send", `{"images":[{"name":"a.png","data":"%%%"}]}`, true); code != http.StatusBadRequest {
		t.Errorf("bad base64 = %d", code)
	}
}

// A document from the page arrives in Files; the history event names it.
func TestWebDocument(t *testing.T) {
	w, srv, c := webServer(t)
	login(t, srv, c, "s3cret-token")
	if code := post(t, c, srv.URL+"/api/send", `{"text":"/add","files":[{"name":"manual.pdf","data":"JVBERi0xLjQ="}]}`, true); code != http.StatusNoContent {
		t.Fatalf("send = %d", code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	m, err := w.Receive(ctx)
	if err != nil || len(m.Files) != 1 || m.Files[0].Name != "manual.pdf" || string(m.Files[0].Data) != "%PDF-1.4" {
		t.Fatalf("receive = %+v, %v", m, err)
	}
	if last := w.history[len(w.history)-1]; len(last.Files) != 1 || last.Files[0] != "manual.pdf" {
		t.Errorf("event = %+v", last)
	}
}

func TestWebPendingState(t *testing.T) {
	ev := func(ts ...string) []webEvent {
		var out []webEvent
		for _, t := range ts {
			out = append(out, webEvent{Type: t})
		}
		return out
	}
	for in, want := range map[string]string{
		"user busy ask":               "busy,ask", // a question is open while the turn runs
		"user busy ask answered":      "busy",
		"user busy ask answered idle": "",
		"busy idle busy":              "busy",
	} {
		var got []string
		for _, e := range pendingState(ev(strings.Fields(in)...)) {
			got = append(got, e.Type)
		}
		if strings.Join(got, ",") != want {
			t.Errorf("%s -> %v, want %s", in, got, want)
		}
	}
}

func TestWebSidebarAPI(t *testing.T) {
	w, srv, c := webServer(t)
	w.Conversations = func(context.Context) ([]ConversationInfo, error) {
		return []ConversationInfo{{ID: "c-1", Label: "[travato] Van oil", Current: true}}, nil
	}
	w.History = func(context.Context) ([]HistoryMessage, error) {
		return []HistoryMessage{{Role: "user", Text: "what oil?"}, {Role: "assistant", Text: "5W-20"}}, nil
	}
	if resp, _ := c.Get(srv.URL + "/api/chats"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("chats without login = %d", resp.StatusCode)
	}
	login(t, srv, c, "s3cret-token")
	for path, want := range map[string]string{"/api/chats": `"label":"[travato] Van oil"`, "/api/history": `"text":"5W-20"`} {
		resp, err := c.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		if body := readAll(resp); !strings.Contains(body, want) {
			t.Errorf("%s = %s", path, body)
		}
	}
}
