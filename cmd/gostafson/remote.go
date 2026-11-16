package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ahplummer/gostafson/internal/media"
	"github.com/ahplummer/gostafson/internal/transport"
)

// remote is `gostafson remote`: the terminal UI for the running service,
// over its web API (the same one the web page uses), so it shares the
// service's conversations, projects, profiles and approvals. It is the web
// chat: what is typed here shows on the web page too, and the other way round.
func remote(args []string) error {
	fs := flag.NewFlagSet("remote", flag.ExitOnError)
	base := fs.String("url", os.Getenv("GOSTAFSON_REMOTE_URL"), "the service's web address, like http://100.64.0.1:8484 (or GOSTAFSON_REMOTE_URL)")
	fs.Parse(args)
	token := os.Getenv("GOSTAFSON_WEB_TOKEN")
	switch {
	case *base == "":
		return errors.New("no address: -url http://<host's tailnet IP>:8484, or set GOSTAFSON_REMOTE_URL")
	case token == "":
		return errors.New("set GOSTAFSON_WEB_TOKEN (the web page's login token, as for make push-secrets)")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	r, err := dialRemote(ctx, strings.TrimRight(*base, "/"), token)
	if err != nil {
		return err
	}
	cli := transport.NewStdio()
	words := []string{"/help", "/new", "/chats", "/chat", "/title", "/delete", "/profiles", "/projects", "/project", "/add", "/status", "/reset", "/exit"}
	if st, err := r.state(ctx); err == nil {
		for _, t := range st.Tags {
			words = append(words, "@"+t)
		}
	}
	cli.Complete = func(string) []string { return words }
	cli.Status = func() string {
		sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		st, err := r.state(sctx)
		if err != nil {
			return "offline"
		}
		var parts []string
		for _, p := range []string{st.Profile, st.Project} {
			if p != "" {
				parts = append(parts, p)
			}
		}
		return strings.Join(parts, " · ")
	}
	cli.Banner(version)
	cli.Note("connected to " + r.base + " as the web chat (it shares the web page's conversation)")
	cli.Note("Tab completes / and @ · ↑ history · Ctrl+V (or ⌘V) pastes a screenshot · /exit or Ctrl+C quits")

	events := make(chan remoteEvent, 64)
	go r.listen(ctx, events)
	for {
		msg, err := cli.Receive(ctx)
		if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
			return nil
		}
		if err != nil {
			return err
		}
		drain(events) // anything from the web page while the prompt was open
		if err := r.send(ctx, msg.Text, msg.Images, msg.Files); err != nil {
			cli.Send(ctx, "", "error: "+err.Error())
			continue
		}
		if err := r.await(ctx, cli, events); err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			cli.Send(ctx, "", "error: "+err.Error())
		}
	}
}

type remoteClient struct {
	base string
	http *http.Client
}

type remoteEvent struct {
	Type string `json:"type"` // user | reply | busy | idle | ask | answered | ready
	Text string `json:"text"`
}

// dialRemote logs in with the token; the session cookie stays in the jar.
func dialRemote(ctx context.Context, base, token string) (*remoteClient, error) {
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequestWithContext(ctx, "POST", base+"/login", strings.NewReader(url.Values{"token": {token}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("can't reach %s: %w", base, err)
	}
	resp.Body.Close()
	if strings.Contains(resp.Header.Get("Location"), "bad=1") {
		return nil, errors.New("login refused: GOSTAFSON_WEB_TOKEN doesn't match the service's web token")
	}
	r := &remoteClient{base: base, http: c}
	if _, err := r.state(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *remoteClient) state(ctx context.Context) (transport.State, error) {
	var st transport.State
	req, _ := http.NewRequestWithContext(ctx, "GET", r.base+"/api/state", nil)
	resp, err := r.http.Do(req)
	if err != nil {
		return st, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return st, fmt.Errorf("%s/api/state: %s (is the service up to date? make deploy)", r.base, resp.Status)
	}
	return st, json.NewDecoder(resp.Body).Decode(&st)
}

func (r *remoteClient) send(ctx context.Context, text string, images, files []media.Image) error {
	type upload struct{ Name, Data string }
	enc := func(ims []media.Image) []upload {
		out := []upload{}
		for _, im := range ims {
			out = append(out, upload{Name: im.Name, Data: base64.StdEncoding.EncodeToString(im.Data)})
		}
		return out
	}
	body, _ := json.Marshal(map[string]any{"text": text, "images": enc(images), "files": enc(files)})
	req, _ := http.NewRequestWithContext(ctx, "POST", r.base+"/api/send", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gostafson", "1")
	resp, err := r.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

// listen reads the event stream, reconnecting when it drops, until ctx ends.
func (r *remoteClient) listen(ctx context.Context, out chan<- remoteEvent) {
	for ctx.Err() == nil {
		req, _ := http.NewRequestWithContext(ctx, "GET", r.base+"/api/events", nil)
		resp, err := r.http.Do(req)
		if err == nil {
			sc := bufio.NewScanner(resp.Body)
			sc.Buffer(make([]byte, 64*1024), 4<<20)
			for sc.Scan() {
				data, ok := strings.CutPrefix(sc.Text(), "data: ")
				if !ok {
					continue
				}
				var e remoteEvent
				if json.Unmarshal([]byte(data), &e) == nil {
					out <- e
				}
			}
			resp.Body.Close()
		}
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
	}
}

func drain(ch <-chan remoteEvent) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// await shows the turn's progress until its reply: the spinner while it runs,
// and approval questions, answered here.
func (r *remoteClient) await(ctx context.Context, cli *transport.CLI, events <-chan remoteEvent) error {
	stopSpin := func() {}
	defer func() { stopSpin() }()
	cancelAsk := func() {}
	defer func() { cancelAsk() }()
	for {
		var e remoteEvent
		select {
		case <-ctx.Done():
			return ctx.Err()
		case e = <-events:
		}
		switch e.Type {
		case "busy":
			stopSpin()
			stopSpin = cli.Busy("")
		case "idle":
			stopSpin()
			stopSpin = func() {}
		case "ask":
			actx, cancel := context.WithCancel(ctx)
			cancelAsk = cancel
			go func() {
				ans, err := cli.Ask(actx, "", e.Text)
				if err == nil {
					if err := r.send(ctx, ans, nil, nil); err != nil {
						cli.Send(ctx, "", "error sending the answer: "+err.Error())
					}
				}
			}()
		case "answered":
			cancelAsk() // answered here, or on the web page, or timed out on the service
		case "reply":
			stopSpin()
			stopSpin = func() {}
			return cli.Send(ctx, "", e.Text)
		}
	}
}
