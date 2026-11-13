// Package router picks the backend profile for one incoming message.
//
// A leading @tag selects the profile with that name and is removed from the
// text. Router.Route handles one message; Sticky adds per-chat memory: a tag
// for a sticky profile (every local one, and remote ones marked sticky) holds
// until another sticky tag, @default or a reset; any other tag applies to its
// message only (PRD D32, D33). Without a
// tag or a sticky profile, the default profile is used (a decision model takes
// over the untagged case in a later milestone). The token think=true anywhere
// in the message turns reasoning on for the turn.
package router

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Route is the routing result for one message.
type Route struct {
	Profile string // profile name
	Text    string // message with the tag and think=true removed
	Think   bool   // think=true was present
	Tagged  bool   // profile came from an explicit @tag
	Sticky  bool   // profile came from the chat's earlier tag (Sticky.Route)
}

// ErrEmpty means nothing was left to send after removing the tag and flags.
// For a message that was only a tag, Route also returns the tagged route.
var ErrEmpty = errors.New("empty message")

// UnknownTagError reports a tag that doesn't match any profile.
type UnknownTagError struct {
	Tag   string
	Valid []string
}

func (e *UnknownTagError) Error() string {
	tags := make([]string, len(e.Valid))
	for i, v := range e.Valid {
		tags[i] = "@" + v
	}
	return fmt.Sprintf("unknown tag @%s; valid tags: %s", e.Tag, strings.Join(tags, ", "))
}

var thinkFlag = regexp.MustCompile(`(?i)(^|\s)think=true\b`)

// Router holds the profile names it can route to.
type Router struct {
	profiles map[string]bool
	def      string
}

// New returns a router over the given profile names. def must be one of them.
func New(profiles []string, def string) (*Router, error) {
	r := &Router{profiles: make(map[string]bool, len(profiles)), def: def}
	for _, p := range profiles {
		r.profiles[p] = true
	}
	if !r.profiles[def] {
		return nil, fmt.Errorf("default profile %q is not in the profile list", def)
	}
	return r, nil
}

// Tags returns the valid tag names, sorted.
func (r *Router) Tags() []string {
	out := make([]string, 0, len(r.profiles))
	for p := range r.profiles {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Route parses one message.
func (r *Router) Route(text string) (Route, error) {
	text = strings.TrimSpace(text)
	route := Route{Profile: r.def}

	if strings.HasPrefix(text, "@") {
		tag, rest, _ := strings.Cut(text[1:], " ")
		// Allow "@code\nmulti-line message" as well as "@code message".
		if i := strings.IndexAny(tag, "\n\t\r"); i >= 0 {
			rest = tag[i:] + " " + rest
			tag = tag[:i]
		}
		tag = strings.ToLower(tag)
		if !r.profiles[tag] {
			return Route{}, &UnknownTagError{Tag: tag, Valid: r.Tags()}
		}
		route.Profile = tag
		route.Tagged = true
		text = rest
	}

	if thinkFlag.MatchString(text) {
		route.Think = true
		text = thinkFlag.ReplaceAllString(text, "$1")
	}

	route.Text = strings.TrimSpace(text)
	if route.Text == "" {
		if route.Tagged {
			return route, ErrEmpty
		}
		return Route{}, ErrEmpty
	}
	return route, nil
}

// Default returns the default profile's name.
func (r *Router) Default() string { return r.def }

// Sticky routes messages per chat, remembering the last sticky profile tagged.
type Sticky struct {
	r      *Router
	sticks func(profile string) bool
	mu     sync.Mutex
	active map[string]string // chat -> sticky profile; absent means the default
}

// NewSticky returns a per-chat router. sticks reports whether a profile's
// tag holds for the chat.
func NewSticky(r *Router, sticks func(profile string) bool) *Sticky {
	return &Sticky{r: r, sticks: sticks, active: map[string]string{}}
}

// Route routes one message in chat. A tagged sticky profile becomes the chat's
// profile (also for a message that is only a tag, which returns ErrEmpty with
// the route). An untagged message goes to the chat's profile.
func (s *Sticky) Route(chat, text string) (Route, error) {
	route, err := s.r.Route(text)
	if err != nil && !(errors.Is(err, ErrEmpty) && route.Tagged) {
		return route, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case route.Tagged && s.sticks(route.Profile):
		if route.Profile == s.r.def {
			delete(s.active, chat)
		} else {
			s.active[chat] = route.Profile
		}
	case !route.Tagged:
		if p, ok := s.active[chat]; ok {
			route.Profile, route.Sticky = p, true
		}
	}
	return route, err
}

// Active returns the profile untagged messages in chat go to.
func (s *Sticky) Active(chat string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.active[chat]; ok {
		return p
	}
	return s.r.def
}

// Reset sends chat back to the default profile.
func (s *Sticky) Reset(chat string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.active, chat)
}
