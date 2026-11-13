package router

import (
	"errors"
	"testing"
)

func TestRoute(t *testing.T) {
	r, err := New([]string{"default", "code", "cloud"}, "default")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		in   string
		want Route
	}{
		{"hello", Route{Profile: "default", Text: "hello"}},
		{"  hello  ", Route{Profile: "default", Text: "hello"}},
		{"@code fix the nav", Route{Profile: "code", Text: "fix the nav", Tagged: true}},
		{"@CODE fix the nav", Route{Profile: "code", Text: "fix the nav", Tagged: true}},
		{"@code\nline one\nline two", Route{Profile: "code", Text: "line one\nline two", Tagged: true}},
		{"@default hi", Route{Profile: "default", Text: "hi", Tagged: true}},
		{"why is the sky blue think=true", Route{Profile: "default", Text: "why is the sky blue", Think: true}},
		{"think=true why", Route{Profile: "default", Text: "why", Think: true}},
		{"@code think=true refactor this", Route{Profile: "code", Text: "refactor this", Think: true, Tagged: true}},
		{"THINK=TRUE why", Route{Profile: "default", Text: "why", Think: true}},
		{"rethink=true is not a flag", Route{Profile: "default", Text: "rethink=true is not a flag"}},
		{"think=trueish is not a flag", Route{Profile: "default", Text: "think=trueish is not a flag"}},
		{"email me@example.com", Route{Profile: "default", Text: "email me@example.com"}},
	}
	for _, tt := range tests {
		got, err := r.Route(tt.in)
		if err != nil {
			t.Errorf("Route(%q) error: %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("Route(%q) = %+v, want %+v", tt.in, got, tt.want)
		}
	}
}

func TestRouteErrors(t *testing.T) {
	r, _ := New([]string{"default", "code"}, "default")

	_, err := r.Route("@nope hi")
	var ute *UnknownTagError
	if !errors.As(err, &ute) || ute.Tag != "nope" {
		t.Fatalf("err = %v", err)
	}
	if want := "unknown tag @nope; valid tags: @code, @default"; err.Error() != want {
		t.Errorf("message = %q, want %q", err.Error(), want)
	}

	for _, in := range []string{"", "   ", "@code", "@code   ", "think=true", "@code think=true"} {
		if _, err := r.Route(in); !errors.Is(err, ErrEmpty) {
			t.Errorf("Route(%q) err = %v, want ErrEmpty", in, err)
		}
	}
}

func TestNewRequiresDefault(t *testing.T) {
	if _, err := New([]string{"code"}, "default"); err == nil {
		t.Error("expected error when default profile is missing")
	}
}

func TestSticky(t *testing.T) {
	r, _ := New([]string{"default", "code", "cloud"}, "default")
	s := NewSticky(r, func(p string) bool { return p != "cloud" })
	step := func(chat, text, wantProfile string, wantSticky bool) {
		t.Helper()
		rt, err := s.Route(chat, text)
		if err != nil && !errors.Is(err, ErrEmpty) {
			t.Fatalf("Route(%q) err = %v", text, err)
		}
		if rt.Profile != wantProfile || rt.Sticky != wantSticky {
			t.Errorf("Route(%q) = %s sticky=%v, want %s sticky=%v", text, rt.Profile, rt.Sticky, wantProfile, wantSticky)
		}
	}
	step("a", "hello", "default", false)
	step("a", "@code read index.html", "code", false)
	step("a", "yes", "code", true)    // the follow-up stays on code
	step("b", "hi", "default", false) // per chat
	step("a", "@cloud news?", "cloud", false)
	step("a", "and more?", "code", true) // cloud never sticks: back to code, not cloud
	step("a", "@default hi", "default", false)
	step("a", "hi again", "default", false)
	step("a", "@code", "code", false) // a tag alone switches
	if s.Active("a") != "code" {
		t.Errorf("active = %s", s.Active("a"))
	}
	step("a", "@cloud", "cloud", false) // a remote tag alone doesn't switch
	step("a", "x", "code", true)
	s.Reset("a")
	step("a", "x", "default", false)
	if _, err := s.Route("a", "@nope x"); err == nil {
		t.Error("unknown tag: no error")
	}
	step("a", "still default", "default", false)
}
