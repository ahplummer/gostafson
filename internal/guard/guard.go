// Package guard checks every tool call before it runs and records it in the
// audit log.
//
// Rules, in order:
//  1. Default deny: a tool name the profile doesn't list is rejected and
//     logged, whether or not the tool exists.
//  2. A "path" argument must be relative and stay inside the profile's root.
//  3. Risky calls (write_file over an existing file) ask the owner first;
//     anything but yes, including no answer, is a deny.
//  4. The tool runs against the profile's os.Root, which enforces the jail
//     (including symlinks) even if rule 2 has a bug.
//
// A denied or failed call returns an error; the loop sends it back to the
// model as the tool result so the model can correct itself.
package guard

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ahplummer/gostafson/internal/approve"
	"github.com/ahplummer/gostafson/internal/audit"
	"github.com/ahplummer/gostafson/internal/client"
	"github.com/ahplummer/gostafson/internal/tools"
)

// Guard holds one profile's policy: its allowed tools and its root.
type Guard struct {
	profile string
	allowed map[string]tools.Tool
	root    *os.Root
	log     *audit.Log
}

// notApproved is what the model is told when a tool call needs approval and
// doesn't get it. Small models paraphrase vague errors ("an access
// restriction"), so it says plainly what happened.
var notApproved = map[approve.Result]string{
	approve.Denied:      "The user said no to this.",
	approve.Timeout:     "The user didn't answer the approval question in time, which counts as no.",
	approve.Unavailable: "This needs the user's approval, and this chat can't ask for it.",
}

// New builds the guard for a profile. Every name in toolNames must be a known
// tool. If any tools are allowed, rootDir is required; it is created (0700)
// if missing and opened as an os.Root.
func New(profile string, toolNames []string, rootDir string, log *audit.Log) (*Guard, error) {
	all := tools.All()
	g := &Guard{profile: profile, allowed: map[string]tools.Tool{}, log: log}
	for _, n := range toolNames {
		t, ok := all[n]
		if !ok {
			return nil, fmt.Errorf("profiles.%s.tools: unknown tool %q; known tools: %s", profile, n, strings.Join(tools.Names(), ", "))
		}
		g.allowed[n] = t
	}
	if len(g.allowed) == 0 {
		return g, nil
	}
	if rootDir == "" {
		return nil, fmt.Errorf("profiles.%s: tools need a root", profile)
	}
	if err := os.MkdirAll(rootDir, 0o700); err != nil {
		return nil, fmt.Errorf("profiles.%s.root: %w", profile, err)
	}
	r, err := os.OpenRoot(rootDir)
	if err != nil {
		return nil, fmt.Errorf("profiles.%s.root: %w", profile, err)
	}
	g.root = r
	return g, nil
}

// Close releases the root.
func (g *Guard) Close() error {
	if g.root == nil {
		return nil
	}
	return g.root.Close()
}

// Tools returns the allowed tool names, sorted.
func (g *Guard) Tools() []string {
	var n []string
	for k := range g.allowed {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

// Root returns the root directory, or "" when the profile has no tools.
func (g *Guard) Root() string {
	if g.root == nil {
		return ""
	}
	return g.root.Name()
}

// Approver asks the owner a yes/no question. *approve.Gate implements it.
type Approver interface {
	Approve(ctx context.Context, chatID, question string) approve.Result
}

// ForTurn returns the tool set for one turn. It implements loop.Tools; its
// audit records carry the turn's trace ID and span ID as parent. A nil
// approver denies every call that needs approval.
func (g *Guard) ForTurn(chat, traceID, turnSpanID string, approver Approver) *Turn {
	return &Turn{g: g, chat: chat, traceID: traceID, parentSpan: turnSpanID, approver: approver}
}

// Turn is a guard bound to one turn.
type Turn struct {
	g          *Guard
	chat       string
	traceID    string
	parentSpan string
	approver   Approver
}

// Definitions returns the schemas of the allowed tools, sorted by name.
func (t *Turn) Definitions() []client.Tool {
	var defs []client.Tool
	for _, n := range t.g.Tools() {
		defs = append(defs, t.g.allowed[n].Def)
	}
	return defs
}

// Call checks and runs one tool call, and writes its audit record.
func (t *Turn) Call(ctx context.Context, name, arguments string) (string, error) {
	start := time.Now()
	rec := audit.ToolCall{
		Time: start.UTC(), Kind: "tool",
		TraceID: t.traceID, SpanID: audit.NewSpanID(), ParentSpanID: t.parentSpan,
		Chat: t.chat, Profile: t.g.profile, Tool: name, Args: audit.Args(arguments),
	}

	out, err := t.run(ctx, name, arguments, &rec)

	rec.DurationMS = time.Since(start).Milliseconds()
	rec.ResultBytes = len(out)
	if err != nil {
		rec.Error = err.Error()
	}
	if werr := t.g.log.Write(rec); werr != nil && err == nil {
		// An unlogged tool call is not allowed to succeed silently.
		return "", fmt.Errorf("audit log: %w", werr)
	}
	return out, err
}

func (t *Turn) run(ctx context.Context, name, arguments string, rec *audit.ToolCall) (string, error) {
	tool, ok := t.g.allowed[name]
	if !ok {
		rec.Guard = "deny"
		if _, exists := tools.All()[name]; exists {
			rec.Reason = "tool not allowed for this profile"
		} else {
			rec.Reason = "unknown tool"
		}
		return "", fmt.Errorf("%s: %q (available: %s)", rec.Reason, name, strings.Join(t.g.Tools(), ", "))
	}

	args := json.RawMessage(arguments)
	if strings.TrimSpace(arguments) == "" {
		args = json.RawMessage("{}")
	}
	var probe struct {
		Path *string `json:"path"`
	}
	if err := json.Unmarshal(args, &probe); err != nil {
		rec.Guard = "deny"
		rec.Reason = "arguments are not a JSON object"
		return "", fmt.Errorf("%s: %v", rec.Reason, err)
	}
	if probe.Path != nil {
		if _, err := tools.Rel(*probe.Path); err != nil {
			rec.Guard = "deny"
			rec.Reason = "path outside root"
			return "", err
		}
	}

	if tool.Check != nil {
		if err := tool.Check(t.g.root, args); err != nil {
			rec.Guard = "deny"
			rec.Reason = "arguments not allowed"
			return "", err
		}
	}

	if tool.Confirm != nil {
		if q := tool.Confirm(t.g.root, args); q != "" {
			result := approve.Unavailable
			if t.approver != nil {
				result = t.approver.Approve(ctx, t.chat, q)
			}
			rec.Approval = string(result)
			if result != approve.Approved {
				rec.Guard = "deny"
				rec.Reason = "not approved"
				return "", fmt.Errorf("%s Nothing was changed. Tell the user plainly that you didn't do it and why; don't retry unless they ask", notApproved[result])
			}
		}
	}

	rec.Guard = "allow"
	return tool.Run(ctx, t.g.root, args)
}
