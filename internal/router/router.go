// Package router picks the backend profile for one incoming message.
//
// A leading @tag selects the profile with that name for this turn only and is
// removed from the text. Without a tag the default profile is used (a decision
// model takes over the untagged case in a later milestone). The token
// think=true anywhere in the message turns reasoning on for the turn.
package router

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Route is the routing result for one message.
type Route struct {
	Profile string // profile name
	Text    string // message with the tag and think=true removed
	Think   bool   // think=true was present
	Tagged  bool   // profile came from an explicit @tag
}

// ErrEmpty means nothing was left to send after removing the tag and flags.
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
		return Route{}, ErrEmpty
	}
	return route, nil
}
