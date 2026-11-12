package transport

import (
	"bufio"
	"context"
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

	// A page that connects now gets the history replayed.
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
	if got := strings.Join(types, ","); got != "user,ask,answered,busy,idle,reply,ready" {
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
