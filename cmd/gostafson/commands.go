package main

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ahplummer/gostafson/internal/client"
)

// command handles a message starting with "/". /exit is handled by the CLI
// transport, not here, so a remote transport can't stop the process.
func (a *app) command(ctx context.Context, chatID, text string) string {
	switch strings.Fields(text)[0] {
	case "/new", "/reset", "/chats", "/chat", "/title", "/delete":
		return a.conversationCommand(ctx, chatID, text)
	case "/profiles":
		return a.profiles()
	case "/projects", "/project":
		return a.projectCommand(ctx, chatID, text)
	case "/status":
		return a.status(ctx, chatID)
	default:
		return a.help(chatID)
	}
}

func (a *app) help(chatID string) string {
	tags := a.router.Tags()
	for i, t := range tags {
		tags[i] = "@" + t
	}
	return fmt.Sprintf(`commands:
  /profiles  tags, models and availability
  /status    version, history size, model servers
  /new       start a new conversation (the current one is saved; /reset does the same)
  /chats     saved conversations; /chats <words> searches them; /chat N picks one up
  /title     rename this conversation; /delete removes it for good
  /projects  list projects; /project <name> attaches one, /project off detaches
  /project cloud on|off   let cloud profiles use the attached project (off by default)
  /project new <name>   create a project; send files with the caption /add to add sources
  /help      this list
  /exit      quit (CLI only; also /quit or Ctrl-D)
tags: %[1]s (no tag: @%[2]s)
  a local tag sticks: later untagged messages in this chat go to the same
  profile until another tag, @%[3]s or /reset; "@code" alone just switches.
  cloud tags apply to their own message only, unless the profile has
  sticky = true in config (see /profiles).
add think=true anywhere in a message to turn on reasoning`, strings.Join(tags, " "), a.sticky.Active(chatID), a.cfg.DefaultProfile)
}

// profiles lists each profile: tag, model, server host, and notes.
func (a *app) profiles() string {
	var b strings.Builder
	for _, name := range a.cfg.ProfileNames() {
		p := a.cfg.Profiles[name]
		var notes []string
		if name == a.cfg.DefaultProfile {
			notes = append(notes, "default")
		}
		if p.ThinkToggle {
			notes = append(notes, "think=true toggle")
		}
		if g := a.guards[name]; len(g.Tools()) > 0 {
			notes = append(notes, fmt.Sprintf("tools: %s in %s", strings.Join(g.Tools(), " "), g.Root()))
		}
		if p.Remote() {
			notes = append(notes, "remote: text-only history")
			if v, ok := p.Provider["data_collection"]; ok {
				notes = append(notes, fmt.Sprintf("data_collection=%v", v))
			}
			if v, ok := p.Provider["zdr"]; ok {
				notes = append(notes, fmt.Sprintf("zdr=%v", v))
			}
			for _, pl := range p.Plugins {
				if pl["id"] == "web" {
					notes = append(notes, "web search")
				}
			}
			if p.Sticky {
				notes = append(notes, "sticky")
			} else {
				notes = append(notes, "one message per tag")
			}
		}
		if p.MissingKey != "" {
			notes = append(notes, "UNAVAILABLE: "+p.MissingKey)
		}
		fmt.Fprintf(&b, "@%-9s %-28s %s", name, p.Model, host(p.BaseURL))
		if len(notes) > 0 {
			fmt.Fprintf(&b, "  (%s)", strings.Join(notes, ", "))
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// status reports version, history size and whether each model server answers.
// Servers are checked in parallel with a short timeout; each distinct base URL
// is checked once.
func (a *app) status(ctx context.Context, chatID string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "gostafson %s\n", version)
	fmt.Fprintf(&b, "default profile: @%s; this chat: @%s\n", a.cfg.DefaultProfile, a.sticky.Active(chatID))
	if p := a.project(chatID); p != nil {
		fmt.Fprintf(&b, "project: %s\n", p.Name)
	}
	if c, err := a.store.Current(ctx, chatID); err != nil {
		fmt.Fprintf(&b, "conversation: error: %v\n", err)
	} else {
		summary := ""
		if c.Summary != "" {
			summary = ", older ones summarized"
		}
		fmt.Fprintf(&b, "conversation: %s, %d messages (last %d sent per turn%s)\n", label(c), c.Messages, a.cfg.Sessions.MaxMessages, summary)
	}

	// One check per base URL; use any profile's client for it (same URL, and
	// a profile with a key so authenticated servers answer).
	type check struct {
		url      string
		profiles []string
		c        *client.Client
		result   string
	}
	var checks []*check
	byURL := map[string]*check{}
	for _, name := range a.cfg.ProfileNames() {
		p := a.cfg.Profiles[name]
		ch, ok := byURL[p.BaseURL]
		if !ok {
			ch = &check{url: p.BaseURL}
			byURL[p.BaseURL] = ch
			checks = append(checks, ch)
		}
		ch.profiles = append(ch.profiles, "@"+name)
		if ch.c == nil || p.ResolvedAPIKey != "" {
			ch.c = a.clients[name]
		}
	}

	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, ch := range checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			if err := ch.c.Ping(pingCtx); err != nil {
				ch.result = "DOWN: " + err.Error()
			} else {
				ch.result = fmt.Sprintf("up (%dms)", time.Since(start).Milliseconds())
			}
		}()
	}
	wg.Wait()

	b.WriteString("model servers:")
	for _, ch := range checks {
		fmt.Fprintf(&b, "\n  %s %s: %s", host(ch.url), strings.Join(ch.profiles, " "), ch.result)
	}
	return b.String()
}

func host(baseURL string) string {
	if u, err := url.Parse(baseURL); err == nil && u.Host != "" {
		return u.Host
	}
	return baseURL
}
