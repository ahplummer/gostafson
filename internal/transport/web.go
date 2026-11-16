package transport

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ahplummer/gostafson/internal/media"
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
	// Conversations and History, set by main, serve the sidebar and the
	// current conversation's messages (PRD D38).
	Conversations func(ctx context.Context) (Sidebar, error)
	History       func(ctx context.Context) ([]HistoryMessage, error)
	// State, set by main, is the chat's profile, project and tags, for
	// `gostafson remote`'s prompt and completion.
	State func(ctx context.Context) (State, error)

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
	Type   string    `json:"type"` // user | reply | busy | idle | ask | answered
	Text   string    `json:"text,omitempty"`
	Images int       `json:"images,omitempty"` // images sent with a user message (the data isn't kept)
	Files  []string  `json:"files,omitempty"`  // names of documents sent with it
	Time   time.Time `json:"time"`
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

// Start listens on the address and serves until ctx ends. If the address
// isn't on any interface yet (at boot, before Tailscale is up), it keeps
// trying every few seconds in the background instead of failing, so the
// other transports still start. Any other listen error is returned.
func (w *Web) Start(ctx context.Context, log *slog.Logger) error {
	ln, err := net.Listen("tcp", w.addr)
	if errors.Is(err, syscall.EADDRNOTAVAIL) {
		log.Warn("web page: address not up yet (Tailscale still starting?); retrying", "addr", w.addr)
		go w.retryListen(ctx, log)
		return nil
	}
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}
	w.serve(ctx, ln)
	return nil
}

// retryInterval is how often Start retries an address that isn't up yet.
var retryInterval = 5 * time.Second

func (w *Web) retryListen(ctx context.Context, log *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(retryInterval):
		}
		ln, err := net.Listen("tcp", w.addr)
		if errors.Is(err, syscall.EADDRNOTAVAIL) {
			continue
		}
		if err != nil {
			w.listenErr <- fmt.Errorf("web: %w", err)
			return
		}
		log.Warn("web page: listening", "addr", w.addr)
		w.serve(ctx, ln)
		return
	}
}

func (w *Web) serve(ctx context.Context, ln net.Listener) {
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
	mux.HandleFunc("GET /api/chats", w.requireAuth(func(rw http.ResponseWriter, r *http.Request) {
		w.serveJSON(rw, r, func(ctx context.Context) (any, error) {
			if w.Conversations == nil {
				return Sidebar{Projects: []string{}, Chats: []ConversationInfo{}}, nil
			}
			return w.Conversations(ctx)
		})
	}))
	mux.HandleFunc("GET /api/state", w.requireAuth(func(rw http.ResponseWriter, r *http.Request) {
		w.serveJSON(rw, r, func(ctx context.Context) (any, error) {
			if w.State == nil {
				return State{Tags: []string{}}, nil
			}
			return w.State(ctx)
		})
	}))
	mux.HandleFunc("GET /api/history", w.requireAuth(func(rw http.ResponseWriter, r *http.Request) {
		w.serveJSON(rw, r, func(ctx context.Context) (any, error) {
			if w.History == nil {
				return []HistoryMessage{}, nil
			}
			return w.History(ctx)
		})
	}))
	return securityHeaders(mux)
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		hd := rw.Header()
		hd.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' blob: data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
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
	backlog := pendingState(w.history)
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
	type upload struct {
		Name string
		Data string // base64, without the data: prefix
	}
	var body struct {
		Text   string
		Images []upload
		Files  []upload // documents (PDF, text) for a project
	}
	// The page scales images down before sending; this limit allows a few
	// at full size anyway.
	if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 48<<20)).Decode(&body); err != nil {
		http.Error(rw, "bad request (too large?)", http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(body.Text)
	decode := func(ups []upload) ([]media.Image, bool) {
		var out []media.Image
		for _, u := range ups {
			data, err := base64.StdEncoding.DecodeString(u.Data)
			if err != nil {
				return nil, false
			}
			out = append(out, media.Image{Name: u.Name, Data: data})
		}
		return out, true
	}
	ims, ok1 := decode(body.Images)
	files, ok2 := decode(body.Files)
	if !ok1 || !ok2 {
		http.Error(rw, "bad file data", http.StatusBadRequest)
		return
	}
	if text == "" && len(ims)+len(files) == 0 {
		rw.WriteHeader(http.StatusNoContent)
		return
	}
	if len(ims)+len(files) == 0 && w.pending.Deliver(WebChat, text) { // an answer: Ask publishes it with the question
		rw.WriteHeader(http.StatusNoContent)
		return
	}
	w.publish(webEvent{Type: "user", Text: text, Images: len(ims), Files: names(files)})
	select {
	case w.in <- Message{ChatID: WebChat, Sender: "web", Text: text, Images: ims, Files: files}:
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
		ans = "(no answer: treated as no)"
	}
	// The page keeps the question and its answer in the chat.
	w.publish(webEvent{Type: "answered", Text: ans})
	if err != nil {
		return "", err
	}
	return ans, nil
}

func names(files []media.Image) []string {
	var out []string
	for _, f := range files {
		out = append(out, f.Name)
	}
	return out
}

// State is the web chat's sticky profile ("" for the default) and attached
// project, and the profile tags.
type State struct {
	Profile string   `json:"profile,omitempty"`
	Project string   `json:"project,omitempty"`
	Tags    []string `json:"tags"`
}

// Sidebar is what the page's sidebar shows: saved conversations and the
// project of the current one, which the page can filter on (PRD D42).
type Sidebar struct {
	Project  string             `json:"project,omitempty"`
	Projects []string           `json:"projects"` // every project, for the filter buttons
	Chats    []ConversationInfo `json:"chats"`
}

// ConversationInfo is one saved conversation in the page's sidebar.
type ConversationInfo struct {
	ID       string `json:"id"`
	Label    string `json:"label"` // "[travato] Van oil"
	Project  string `json:"project,omitempty"`
	When     string `json:"when"`
	Profile  string `json:"profile,omitempty"`
	Messages int    `json:"messages"`
	Current  bool   `json:"current"`
}

// HistoryMessage is one message of the current conversation, for drawing
// the page.
type HistoryMessage struct {
	Role   string `json:"role"` // user | assistant
	Text   string `json:"text"`
	Images int    `json:"images,omitempty"`
}

func (w *Web) serveJSON(rw http.ResponseWriter, r *http.Request, get func(context.Context) (any, error)) {
	v, err := get(r.Context())
	if err != nil {
		http.Error(rw, "error", http.StatusInternalServerError)
		return
	}
	rw.Header().Set("Content-Type", "application/json")
	json.NewEncoder(rw).Encode(v)
}

// pendingState is what a page that connects needs from the event history:
// an approval question still open, and whether a turn is running. The
// messages themselves come from the saved conversation (/api/history), so
// they are drawn once, from one place (PRD D38).
func pendingState(events []webEvent) []webEvent {
	var out []webEvent
	var ask, busy *webEvent
	for i := range events {
		switch e := &events[i]; e.Type {
		case "ask":
			ask = e
		case "answered":
			ask = nil
		case "busy":
			busy = e
		case "idle":
			busy = nil
		}
	}
	if busy != nil {
		out = append(out, *busy)
	}
	if ask != nil {
		out = append(out, *ask)
	}
	return out
}
