package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ahplummer/gostafson/internal/transport"
)

// An approval question during a turn is asked in the terminal and the
// answer is posted back; the turn ends at its reply.
func TestRemoteAwaitAnswersQuestion(t *testing.T) {
	var mu sync.Mutex
	var posted []string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var body struct{ Text string }
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		posted = append(posted, body.Text)
		mu.Unlock()
		rw.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	r := &remoteClient{base: srv.URL, http: srv.Client()}

	in, inW := io.Pipe()
	var out bytes.Buffer
	cli := transport.NewCLI(in, &out)
	events := make(chan remoteEvent, 8)
	events <- remoteEvent{Type: "busy"}
	events <- remoteEvent{Type: "ask", Text: "overwrite a.txt?"}
	go func() {
		inW.Write([]byte("yes\n"))
		time.Sleep(200 * time.Millisecond) // the answer is posted before the service replies
		events <- remoteEvent{Type: "answered", Text: "yes"}
		events <- remoteEvent{Type: "idle"}
		events <- remoteEvent{Type: "reply", Text: "done"}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.await(ctx, cli, events); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(posted) != 1 || posted[0] != "yes" {
		t.Errorf("posted %q, want the answer yes", posted)
	}
	if s := out.String(); !strings.Contains(s, "overwrite a.txt?") || !strings.Contains(s, "done") {
		t.Errorf("terminal = %q", s)
	}
}
