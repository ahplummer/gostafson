package transport

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

//go:embed webpage
var webFiles embed.FS

// WebChat is the chat ID for messages from the web page. One owner, one chat.
const WebChat = "web"

const cookieName = "gostafson"

// Web serves a chat page and a small JSON API on a private address (loopback
// or the tailnet; config enforces that). Every request except the login form
// needs the session cookie, which is an HMAC of the configured token, so a
// restart doesn't log the phone out and the token itself is never stored in
// the browser.
type Web struct {
	addr    string
	token   string
	session string // expected cookie value

	in      chan Message
	pending Pending

	mu      sync.Mutex
	subs    map[chan webEvent]struct{}
	history []webEvent // recent events, replayed to a page when it connects

	listenErr chan error
}

type webEvent struct {
	Type string    `json:"type"` // user | reply | busy | idle | ask | answered
	Text string    `json:"text,omitempty"`
	Time time.Time `json:"time"`
}

const maxHistory = 200

// NewWeb returns a web transport for addr, using token for login.
func NewWeb(addr, token string) *Web {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte("gostafson web session v1"))
	return &Web{
		addr: addr, token: token, session: hex.EncodeToString(mac.Sum(nil)),
		in: make(chan Message, 16), subs: map[chan webEvent]struct{}{},
		listenErr: make(chan error, 1),
	}
}

// Name implements Transport.
func (w *Web) Name() string { return "web" }

// Start listens on the address and serves until ctx ends.
func (w *Web) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", w.addr)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}
	srv := &http.Server{Handler: w.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			w.listenErr <- err
		}
	}()
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	return nil
}

// Handler returns the HTTP routes. Exposed for tests.
func (w *Web) Handler() http.Handler {
	static, _ := fs.Sub(webFiles, "webpage")
	files := http.FileServerFS(static)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(rw http.ResponseWriter, r *http.Request) {
		page := "index.html"
		if !w.authed(r) {
			page = "login.html"
		}
		http.ServeFileFS(rw, r, static, page)
	})
	mux.HandleFunc("POST /login", w.login)
	mux.HandleFunc("GET /logout", func(rw http.ResponseWriter, r *http.Request) {
		http.SetCookie(rw, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
		http.Redirect(rw, r, "/", http.StatusSeeOther)
	})
	mux.Handle("GET /app.js", files)
	mux.Handle("GET /app.css", files)
	mux.HandleFunc("GET /api/events", w.requireAuth(w.events))
	mux.HandleFunc("POST /api/send", w.requireAuth(w.send))
	return securityHeaders(mux)
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		hd := rw.Header()
		hd.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("Cache-Control", "no-store")
		h.ServeHTTP(rw, r)
	})
}

func (w *Web) authed(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	return err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(w.session)) == 1
}

// requireAuth checks the cookie, and for POSTs a custom header that a form
// on another site can't send (with SameSite=Strict, a second CSRF guard).
func (w *Web) requireAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		if !w.authed(r) {
			http.Error(rw, "log in first", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost && r.Header.Get("X-Gostafson") != "1" {
			http.Error(rw, "missing X-Gostafson header", http.StatusForbidden)
			return
		}
		h(rw, r)
	}
}

func (w *Web) login(rw http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(rw, r.Body, 4096)
	given := r.FormValue("token")
	if subtle.ConstantTimeCompare([]byte(given), []byte(w.token)) != 1 {
		time.Sleep(time.Second) // slow down guessing
		http.Redirect(rw, r, "/?bad=1", http.StatusSeeOther)
		return
	}
	http.SetCookie(rw, &http.Cookie{
		Name: cookieName, Value: w.session, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 90 * 24 * 3600,
	})
	http.Redirect(rw, r, "/", http.StatusSeeOther)
}

// events streams webEvents as server-sent events, starting with recent history.
func (w *Web) events(rw http.ResponseWriter, r *http.Request) {
	flusher, ok := rw.(http.Flusher)
	if !ok {
		http.Error(rw, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	rw.Header().Set("Content-Type", "text/event-stream")
	ch := make(chan webEvent, 64)
	w.mu.Lock()
	backlog := append([]webEvent(nil), w.history...)
	w.subs[ch] = struct{}{}
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		delete(w.subs, ch)
		w.mu.Unlock()
	}()

	write := func(e webEvent) bool {
		b, _ := json.Marshal(e)
		if _, err := fmt.Fprintf(rw, "data: %s\n\n", b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	for _, e := range backlog {
		if !write(e) {
			return
		}
	}
	write(webEvent{Type: "ready", Time: time.Now()})
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-ch:
			if !write(e) {
				return
			}
		case <-keepalive.C:
			if _, err := fmt.Fprint(rw, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (w *Web) send(rw http.ResponseWriter, r *http.Request) {
	var body struct{ Text string }
	if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 64<<10)).Decode(&body); err != nil {
		http.Error(rw, "bad request", http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		rw.WriteHeader(http.StatusNoContent)
		return
	}
	w.publish(webEvent{Type: "user", Text: text})
	if w.pending.Deliver(WebChat, text) {
		w.publish(webEvent{Type: "answered", Text: text})
		rw.WriteHeader(http.StatusNoContent)
		return
	}
	select {
	case w.in <- Message{ChatID: WebChat, Sender: "web", Text: text}:
		rw.WriteHeader(http.StatusNoContent)
	default:
		http.Error(rw, "too many messages waiting", http.StatusTooManyRequests)
	}
}

// publish records an event and sends it to every open page. A page that
// can't keep up misses live events; it gets them from history on reconnect.
func (w *Web) publish(e webEvent) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.history = append(w.history, e)
	if len(w.history) > maxHistory {
		w.history = w.history[len(w.history)-maxHistory:]
	}
	for ch := range w.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

// Receive implements Transport.
func (w *Web) Receive(ctx context.Context) (Message, error) {
	select {
	case <-ctx.Done():
		return Message{}, ctx.Err()
	case err := <-w.listenErr:
		return Message{}, fmt.Errorf("web: %w", err)
	case m := <-w.in:
		return m, nil
	}
}

// Send implements Transport.
func (w *Web) Send(_ context.Context, _ string, text string) error {
	w.publish(webEvent{Type: "reply", Text: text})
	return nil
}

// Busy implements Indicator: the page shows a spinner.
func (w *Web) Busy(string) func() {
	w.publish(webEvent{Type: "busy"})
	return func() { w.publish(webEvent{Type: "idle"}) }
}

// Ask implements Asker: the page shows the question with yes/no buttons; the
// next message from the page is the answer.
func (w *Web) Ask(ctx context.Context, chatID, question string) (string, error) {
	w.publish(webEvent{Type: "ask", Text: question})
	ans, err := w.pending.Wait(ctx, chatID)
	if err != nil {
		w.publish(webEvent{Type: "answered", Text: "(no answer: treated as no)"})
	}
	return ans, err
}
