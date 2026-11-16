package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ahplummer/gostafson/internal/client"
	"github.com/ahplummer/gostafson/internal/projects"
	"github.com/ahplummer/gostafson/internal/sessions"
	"github.com/ahplummer/gostafson/internal/transport"
)

// Saved conversations (PRD D38): /new starts one and keeps the old; /chats
// lists them (with a search); /chat N picks one up, from any transport, with
// its profile and project; titles and summaries are written by the default
// (local) model in the background.

// conversation returns chat's current conversation and, when it isn't the
// one already loaded for chat, restores its sticky profile and project.
func (a *app) conversation(ctx context.Context, chat string) (sessions.Conversation, error) {
	c, err := a.store.Current(ctx, chat)
	if err != nil {
		return c, err
	}
	a.projMu.Lock()
	fresh := a.loaded[chat] != c.ID
	a.loaded[chat] = c.ID
	if fresh {
		if c.Project != "" {
			a.attached[chat] = c.Project
		} else {
			delete(a.attached, chat)
		}
	}
	a.projMu.Unlock()
	if fresh {
		if _, ok := a.cfg.Profiles[c.Profile]; ok {
			a.sticky.Set(chat, c.Profile)
		} else {
			a.sticky.Set(chat, "") // a profile since removed from the config
		}
		if p, local := a.localOnly(chat); local && a.cfg.Profiles[a.sticky.Active(chat)].Remote() {
			a.sticky.Set(chat, a.projectProfile(p)) // cloud access was turned off since
		}
	}
	return c, nil
}

// saveState stores chat's sticky profile and project with its conversation.
func (a *app) saveState(ctx context.Context, chat, convID string) {
	profile := a.sticky.Active(chat)
	if profile == a.cfg.DefaultProfile {
		profile = ""
	}
	a.projMu.Lock()
	project := a.attached[chat]
	a.projMu.Unlock()
	if err := a.store.SetState(ctx, convID, profile, project); err != nil {
		a.log.Warn("save conversation state", "err", err)
	}
	if project == "" {
		return
	}
	if p, err := a.projects.Open(project); err == nil { // the project's profile follows (PRD D42)
		st, _ := p.Settings()
		if prof, ok := a.cfg.Profiles[a.sticky.Active(chat)]; ok && !(prof.Remote() && !st.Cloud) {
			if err := p.SetProfile(profile); err != nil {
				a.log.Warn("save project profile", "project", project, "err", err)
			}
		}
	}
}

// switchTo makes id chat's current conversation and loads its state.
func (a *app) switchTo(ctx context.Context, chat, id string) (sessions.Conversation, error) {
	if err := a.store.Switch(ctx, chat, id); err != nil {
		return sessions.Conversation{}, err
	}
	return a.conversation(ctx, chat)
}

// label is how a conversation is shown: "[travato] Van oil".
func label(c sessions.Conversation) string {
	t := c.Title
	if t == "" {
		t = "(untitled)"
	}
	if c.Project != "" {
		t = "[" + c.Project + "] " + t
	}
	return t
}

func when(t time.Time) string {
	t = t.Local()
	now := time.Now()
	switch {
	case t.Year() == now.Year() && t.YearDay() == now.YearDay():
		return "today " + t.Format("15:04")
	case t.Year() == now.Year() && t.YearDay() == now.YearDay()-1:
		return "yesterday " + t.Format("15:04")
	case t.Year() == now.Year():
		return t.Format("Jan 2")
	}
	return t.Format("Jan 2 2006")
}

// conversationCommand handles /new, /reset, /chats, /chat, /title, /delete and /move.
func (a *app) conversationCommand(ctx context.Context, chat, text string) string {
	f := strings.Fields(text)
	rest := strings.TrimSpace(strings.TrimPrefix(text, f[0]))
	switch f[0] {
	case "/new", "/reset":
		a.projMu.Lock()
		project := a.attached[chat] // a new conversation stays in the project, like Claude Desktop's
		a.projMu.Unlock()
		if _, err := a.store.New(ctx, chat); err != nil {
			return "error: " + err.Error()
		}
		a.sticky.Reset(chat)
		if _, err := a.conversation(ctx, chat); err != nil {
			return "error: " + err.Error()
		}
		if p, err := a.projects.Open(project); project != "" && err == nil {
			a.sticky.Set(chat, a.projectProfile(p))
			a.setProject(chat, project)
			return fmt.Sprintf("new conversation in %s on @%s; the previous one is saved (/chats lists them, /project off detaches)", project, a.sticky.Active(chat))
		}
		return fmt.Sprintf("new conversation on @%s; the previous one is saved (/chats lists them)", a.cfg.DefaultProfile)

	case "/chats":
		list, err := a.store.List(ctx, rest, 15)
		if err != nil {
			return "error: " + err.Error()
		}
		if len(list) == 0 {
			if rest != "" {
				return "no conversation mentions " + rest
			}
			return "no saved conversations yet"
		}
		cur, _ := a.store.Current(ctx, chat)
		var ids []string
		var b strings.Builder
		if rest != "" {
			fmt.Fprintf(&b, "conversations mentioning %q:\n", rest)
		}
		for i, c := range list {
			ids = append(ids, c.ID)
			mark := "" // no padding: Telegram trims a reply's leading spaces, so the rows wouldn't line up
			if c.ID == cur.ID {
				mark = "* "
			}
			extra := ""
			if c.Profile != "" {
				extra = " · @" + c.Profile
			}
			fmt.Fprintf(&b, "%s%d. %s · %s%s · %d messages\n", mark, i+1, label(c), when(c.Updated), extra, c.Messages)
		}
		a.projMu.Lock()
		a.listing[chat] = ids
		a.projMu.Unlock()
		b.WriteString("/chat N picks one up; /chats <words> searches")
		return b.String()

	case "/chat":
		if rest == "" {
			c, _ := a.store.Current(ctx, chat)
			return "this conversation: " + label(c) + " (/chats lists the others)"
		}
		id := rest
		if n, err := strconv.Atoi(rest); err == nil {
			a.projMu.Lock()
			ids := a.listing[chat]
			a.projMu.Unlock()
			if ids == nil { // no /chats yet in this chat: number the current list
				list, _ := a.store.List(ctx, "", 15)
				for _, c := range list {
					ids = append(ids, c.ID)
				}
			}
			if n < 1 || n > len(ids) {
				return fmt.Sprintf("no conversation %d; /chats lists them", n)
			}
			id = ids[n-1]
		}
		if _, err := a.store.Get(ctx, id); err != nil {
			return "no conversation " + id + "; /chats lists them"
		}
		c, err := a.switchTo(ctx, chat, id)
		if err != nil {
			return "error: " + err.Error()
		}
		var b strings.Builder
		fmt.Fprintf(&b, "back in %s (%s, %d messages)", label(c), when(c.Updated), c.Messages)
		if c.Profile != "" {
			fmt.Fprintf(&b, " on @%s", c.Profile)
		}
		if last := a.lastExchange(ctx, c.ID); last != "" {
			b.WriteString("\n" + last)
		}
		return b.String()

	case "/title":
		c, err := a.store.Current(ctx, chat)
		if err != nil {
			return "error: " + err.Error()
		}
		if rest == "" {
			return "this conversation: " + label(c) + "; /title <words> renames it"
		}
		a.store.SetTitle(ctx, c.ID, rest)
		return "renamed to " + rest

	case "/delete":
		c, err := a.store.Current(ctx, chat)
		if err != nil {
			return "error: " + err.Error()
		}
		if rest != "yes" {
			return fmt.Sprintf("this deletes %s (%d messages) for good; /delete yes to go ahead, or /new to start another and keep it", label(c), c.Messages)
		}
		if err := a.store.Delete(ctx, c.ID); err != nil {
			return "error: " + err.Error()
		}
		a.images.Reset(c.ID)
		a.conversationCommand(ctx, chat, "/new")
		return "deleted " + label(c) + "; this is a new conversation"

	case "/move":
		return a.move(ctx, chat, f[1:])
	}
	return a.help(chat)
}

// move files a conversation under another project, or none (PRD D42):
// "/move <project|none> [conversation id] [yes]". Without an id it moves the
// current conversation and attaches the project to the chat. Search results
// from the old project's sources are in the history, so moving them along
// needs "yes".
func (a *app) move(ctx context.Context, chat string, args []string) string {
	if len(args) == 0 {
		return "usage: /move <project> files this conversation under another project; /move none removes its project"
	}
	target := strings.ToLower(args[0])
	if target == "none" || target == "off" {
		target = ""
	}
	cur, err := a.store.Current(ctx, chat)
	if err != nil {
		return "error: " + err.Error()
	}
	c, yes := cur, false
	for _, w := range args[1:] {
		switch {
		case w == "yes":
			yes = true
		case strings.HasPrefix(w, "c-"):
			if c, err = a.store.Get(ctx, w); err != nil {
				return "no conversation " + w + "; /chats lists them"
			}
		}
	}
	var p *projects.Project
	if target != "" {
		if p, err = a.projects.Open(target); err != nil {
			return err.Error()
		}
	}
	if c.Project == target {
		return label(c) + " is already there"
	}
	if c.Project != "" && !yes {
		if n := a.projectToolResults(ctx, c.ID); n > 0 {
			again := "/move " + args[0]
			if c.ID != cur.ID {
				again += " " + c.ID
			}
			return fmt.Sprintf("%s has %d search or read results from %s's sources in its history; moving it takes them along. %s yes to move anyway",
				label(c), n, c.Project, again)
		}
	}
	dest := "no project"
	if p != nil {
		dest = p.Name
	}
	if c.ID != cur.ID { // another conversation: only its record changes
		profile := c.Profile
		if prof, ok := a.cfg.Profiles[profile]; ok && prof.Remote() && !a.cloudOK(p) {
			profile = ""
		}
		if err := a.store.SetState(ctx, c.ID, profile, target); err != nil {
			return "error: " + err.Error()
		}
		return fmt.Sprintf("moved %s to %s", label(c), dest)
	}
	note := ""
	if prof := a.cfg.Profiles[a.sticky.Active(chat)]; prof.Remote() && !a.cloudOK(p) {
		a.sticky.Set(chat, a.projectProfile(p))
		note = fmt.Sprintf("; %s is local-only, so this chat moved from @%s to @%s", p.Name, prof.Name, a.sticky.Active(chat))
	}
	a.setProject(chat, target) // saves the conversation's project and profile
	return fmt.Sprintf("moved %s to %s%s", label(c), dest, note)
}

// cloudOK reports whether cloud profiles may be used with p (nil: no project).
func (a *app) cloudOK(p *projects.Project) bool {
	if p == nil {
		return true
	}
	st, _ := p.Settings()
	return st.Cloud
}

// projectToolResults counts a conversation's project_search and project_read
// calls, whose results are stored in its history.
func (a *app) projectToolResults(ctx context.Context, id string) int {
	msgs, _ := a.store.Recent(ctx, id, 100000)
	n := 0
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			if tc.Function.Name == "project_search" || tc.Function.Name == "project_read" {
				n++
			}
		}
	}
	return n
}

// projectChats lists project's recent conversations, numbered for /chat N.
func (a *app) projectChats(ctx context.Context, chat, project string) string {
	list, err := a.store.InProject(ctx, project, 5)
	if err != nil || len(list) == 0 {
		return ""
	}
	var ids []string
	var b strings.Builder
	fmt.Fprintf(&b, "%s's recent conversations:\n", project)
	for i, c := range list {
		ids = append(ids, c.ID)
		t := c.Title
		if t == "" {
			t = "(untitled)"
		}
		fmt.Fprintf(&b, "%d. %s · %s · %d messages\n", i+1, t, when(c.Updated), c.Messages)
	}
	a.projMu.Lock()
	a.listing[chat] = ids
	a.projMu.Unlock()
	b.WriteString("/chat N picks one up")
	return b.String()
}

// firstExchange returns the first user message and the reply that ended its
// turn (the last assistant text before the next user message).
func firstExchange(msgs []client.Message) (user, reply string) {
	for _, m := range msgs {
		switch {
		case m.Role == "user" && user == "":
			user = m.Content
		case m.Role == "user":
			return user, reply
		case m.Role == "assistant" && user != "" && len(m.ToolCalls) == 0 && strings.TrimSpace(m.Content) != "":
			reply = m.Content
		}
	}
	return user, reply
}

func cut(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		s = string(r[:n]) + "…"
	}
	return s
}

// lastExchange shows where a resumed conversation left off.
func (a *app) lastExchange(ctx context.Context, id string) string {
	msgs, _ := a.store.Recent(ctx, id, 60) // enough to reach the user message of a turn with many tool calls
	var lastUser, lastReply string
	for _, m := range sessions.TextOnly(msgs) {
		switch m.Role {
		case "user":
			lastUser = m.Content
		case "assistant":
			lastReply = m.Content
		}
	}
	short := func(s string) string {
		s = strings.Join(strings.Fields(s), " ")
		if r := []rune(s); len(r) > 160 {
			s = string(r[:160]) + "…"
		}
		return s
	}
	if lastUser == "" {
		return ""
	}
	out := "last: you: " + short(lastUser)
	if lastReply != "" {
		out += "\n      reply: " + short(lastReply)
	}
	return out
}

// afterTurn titles a new conversation and summarizes what fell out of the
// recent window, with the default (local) model. It runs after the reply
// is sent; errors are logged and the next turn tries again.
func (a *app) afterTurn(id string) {
	a.bgMu.Lock()
	defer a.bgMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c, err := a.store.Get(ctx, id)
	if err != nil {
		return
	}
	if c.Title == "" {
		// From the first question and its answer. Not the last few messages:
		// a turn that used tools ends in several tool messages, and Recent
		// drops everything before a user message, so the model was asked to
		// title an empty transcript and made one up.
		msgs, _ := a.store.Recent(ctx, id, 60)
		if user, reply := firstExchange(msgs); user != "" {
			title, err := a.ask(ctx, "Write a title of 3 to 6 words for this conversation. Reply with the title only, no quotes or punctuation at the end.\n\n"+
				"user: "+cut(user, 1500)+"\nassistant: "+cut(reply, 1500))
			if err == nil && title != "" {
				a.store.SetTitle(ctx, id, strings.Split(title, "\n")[0])
			} else if err != nil {
				a.log.Info("conversation title", "err", err)
			}
		}
	}
	old, err := a.store.Unsummarized(ctx, id, a.cfg.Sessions.MaxMessages)
	if err != nil || len(old) < sessions.SummaryBatch { // in batches; until then they are still sent
		return
	}
	prompt := "Update the running summary of a conversation. Keep the facts, decisions, names, numbers, preferences and open questions " +
		"someone would need to pick it up weeks later. Under 250 words, plain text.\n\n"
	if c.Summary != "" {
		prompt += "Summary so far:\n" + c.Summary + "\n\n"
	}
	prompt += "Messages to add:\n" + sessions.TranscriptText(old)
	summary, err := a.ask(ctx, prompt)
	if err != nil || summary == "" {
		a.log.Info("conversation summary", "err", err)
		return
	}
	a.store.SetSummary(ctx, id, summary, old[len(old)-1].ID)
}

// ask sends one prompt to the default profile's model, thinking off.
func (a *app) ask(ctx context.Context, prompt string) (string, error) {
	p := a.cfg.Profiles[a.cfg.DefaultProfile]
	req := client.Request{Model: p.Model, MaxTokens: 600, Messages: []client.Message{{Role: "user", Content: prompt}}}
	if p.ThinkToggle {
		req.ReasoningEffort = "none"
	}
	r, err := a.clients[a.cfg.DefaultProfile].Chat(ctx, req)
	return strings.TrimSpace(r.Message.Content), err
}

// webConversations lists saved conversations for the web page's sidebar.
// The page filters to the current project's conversations itself, so the
// list is long enough to hold a project's older ones.
func (a *app) webConversations(ctx context.Context) (transport.Sidebar, error) {
	out := transport.Sidebar{Projects: []string{}, Chats: []transport.ConversationInfo{}}
	cur, err := a.store.Current(ctx, transport.WebChat)
	if err != nil {
		return out, err
	}
	out.Project = cur.Project
	if names, err := a.projects.List(); err == nil && names != nil {
		out.Projects = names
	}
	list, err := a.store.List(ctx, "", 200)
	if err != nil {
		return out, err
	}
	for _, c := range list {
		out.Chats = append(out.Chats, transport.ConversationInfo{ID: c.ID, Label: label(c), Project: c.Project, When: when(c.Updated),
			Profile: c.Profile, Messages: c.Messages, Current: c.ID == cur.ID})
	}
	return out, nil
}

// webState is the web chat's profile, project and tags, for `gostafson remote`.
func (a *app) webState(ctx context.Context) (transport.State, error) {
	if _, err := a.conversation(ctx, transport.WebChat); err != nil {
		return transport.State{}, err
	}
	st := transport.State{Tags: a.router.Tags()}
	if p := a.sticky.Active(transport.WebChat); p != a.cfg.DefaultProfile {
		st.Profile = p
	}
	if p := a.project(transport.WebChat); p != nil {
		st.Project = p.Name
	}
	return st, nil
}

// webHistory returns the web page's current conversation for drawing:
// what the user and the assistant said, not tool calls or results.
func (a *app) webHistory(ctx context.Context) ([]transport.HistoryMessage, error) {
	if _, err := a.conversation(ctx, transport.WebChat); err != nil {
		return nil, err
	}
	cur, _ := a.store.Current(ctx, transport.WebChat)
	msgs, err := a.store.Recent(ctx, cur.ID, 400)
	if err != nil {
		return nil, err
	}
	out := []transport.HistoryMessage{}
	for _, m := range msgs {
		switch {
		case m.Role == "user":
			out = append(out, transport.HistoryMessage{Role: "user", Text: m.Content, Images: len(m.ImageRefs)})
		case m.Role == "assistant" && strings.TrimSpace(m.Content) != "":
			out = append(out, transport.HistoryMessage{Role: "assistant", Text: m.Content})
		}
	}
	return out, nil
}
