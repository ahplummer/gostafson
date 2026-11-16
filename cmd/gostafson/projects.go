package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ahplummer/gostafson/internal/approve"
	"github.com/ahplummer/gostafson/internal/client"
	"github.com/ahplummer/gostafson/internal/media"
	"github.com/ahplummer/gostafson/internal/projects"
	"github.com/ahplummer/gostafson/internal/transport"
)

// Projects (PRD D37): a chat attaches one with /project <name>; it stays
// attached until /project off, and /new conversations keep it (D38).
// Each project keeps its profile: attaching one switches the chat to the
// profile the project last used, and a project without cloud access never
// stays on a cloud profile (D42). Its turns get the project's
// instructions, memory and source list in the system prompt, and its search
// tools; cloud profiles get them only when the project has cloud = true.

// describeImage asks the default profile's model for a one-line description
// of an image being indexed, so a screenshot is found by what it shows.
func (a *app) describeImage(ctx context.Context, path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	im, err := media.Prepare(ctx, path, data)
	if err != nil {
		return "", err
	}
	p := a.cfg.Profiles[a.cfg.DefaultProfile]
	if !a.canSee(ctx, a.cfg.DefaultProfile) {
		return "", fmt.Errorf("@%s can't read images", a.cfg.DefaultProfile)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req := client.Request{Model: p.Model, MaxTokens: 150, Messages: []client.Message{{Role: "user", Images: []media.Image{im},
		Content: "Describe this image in one or two sentences for a search index: what it shows, and any product, part or model names."}}}
	if p.ThinkToggle {
		req.ReasoningEffort = "none"
	}
	r, err := a.clients[a.cfg.DefaultProfile].Chat(ctx, req)
	return strings.TrimSpace(r.Message.Content), err
}

func (a *app) project(chat string) *projects.Project {
	a.projMu.Lock()
	name := a.attached[chat]
	a.projMu.Unlock()
	if name == "" {
		return nil
	}
	p, err := a.projects.Open(name)
	if err != nil {
		return nil
	}
	return p
}

// projectProfile is the profile a chat switches to on entering p: the one
// the project last used, or the default; never a cloud profile when the
// project is local-only.
func (a *app) projectProfile(p *projects.Project) string {
	st, _ := p.Settings()
	prof, ok := a.cfg.Profiles[st.Profile]
	if !ok || !prof.Sticks() || (prof.Remote() && !st.Cloud) {
		return a.cfg.DefaultProfile
	}
	return st.Profile
}

// localOnly reports whether chat's project keeps cloud profiles out.
func (a *app) localOnly(chat string) (*projects.Project, bool) {
	p := a.project(chat)
	if p == nil {
		return nil, false
	}
	st, _ := p.Settings()
	return p, !st.Cloud
}

// freshFor starts a new conversation when chat's current one has messages
// and belongs to a different project (or none), so one project's material in
// the history doesn't carry into another (PRD D42). It returns a line saying
// so, or "".
func (a *app) freshFor(ctx context.Context, chat, project string) string {
	c, err := a.store.Current(ctx, chat)
	if err != nil || c.Messages == 0 || c.Project == project {
		return ""
	}
	if _, err := a.store.New(ctx, chat); err != nil {
		return ""
	}
	if _, err := a.conversation(ctx, chat); err != nil {
		return ""
	}
	return fmt.Sprintf("new conversation; %s is saved (/chats lists it)", label(c))
}

// enterProject attaches p to chat, switches the chat to p's profile and
// says which model the chat is on now. was is the profile before.
func (a *app) enterProject(chat string, p *projects.Project, was string) string {
	to := a.projectProfile(p)
	a.sticky.Set(chat, to)
	a.setProject(chat, p.Name)
	where := a.cfg.Profiles[to].Model
	if a.cfg.Profiles[to].Remote() {
		where += ", off this machine"
	}
	if was == to {
		return fmt.Sprintf("model: @%s (%s)", to, where)
	}
	return fmt.Sprintf("model switched to @%s (%s) from @%s: %s uses @%s", to, where, was, p.Name, to)
}

func (a *app) setProject(chat, name string) {
	a.projMu.Lock()
	if name == "" {
		delete(a.attached, chat)
	} else {
		a.attached[chat] = name
	}
	conv := a.loaded[chat]
	a.projMu.Unlock()
	if conv != "" { // saved with the conversation (PRD D38)
		a.saveState(context.Background(), chat, conv)
	}
}

// projectCommand handles /projects and /project ...
func (a *app) projectCommand(ctx context.Context, chat, text string) string {
	f := strings.Fields(text)
	if f[0] == "/projects" || len(f) == 1 {
		return a.listProjects(ctx, chat)
	}
	was := a.sticky.Active(chat)
	switch arg := strings.ToLower(f[1]); arg {
	case "off", "none":
		if fresh := a.freshFor(ctx, chat, ""); fresh != "" {
			return fmt.Sprintf("project detached; %s on @%s", fresh, a.sticky.Active(chat))
		}
		a.setProject(chat, "")
		return "project detached"
	case "cloud":
		p := a.project(chat)
		if p == nil {
			return "attach a project first: /project <name>"
		}
		if len(f) < 3 || (f[2] != "on" && f[2] != "off") {
			st, _ := p.Settings()
			return fmt.Sprintf("cloud access for %s is %s; /project cloud on|off changes it", p.Name, map[bool]string{true: "on", false: "off"}[st.Cloud])
		}
		if err := p.SetCloud(f[2] == "on"); err != nil {
			return "error: " + err.Error()
		}
		if f[2] == "off" {
			if prof := a.cfg.Profiles[a.sticky.Active(chat)]; prof.Remote() {
				a.sticky.Set(chat, a.cfg.DefaultProfile)
				a.setProject(chat, p.Name) // saves the new profile
				return fmt.Sprintf("cloud access off: cloud profiles no longer see %s, and this chat moved from @%s to @%s", p.Name, prof.Name, a.cfg.DefaultProfile)
			}
		}
		if f[2] == "on" {
			return fmt.Sprintf("cloud access on: cloud profiles (@sonnet, @opus, ...) now get %s's instructions, memory and passages, and its search, read and remember tools; that text goes to OpenRouter", p.Name)
		}
		return fmt.Sprintf("cloud access off: cloud profiles no longer see %s", p.Name)
	case "new":
		if len(f) < 3 {
			return "usage: /project new <name>"
		}
		p, err := a.projects.Create(strings.ToLower(f[2]))
		if err != nil {
			return err.Error()
		}
		fresh := a.freshFor(ctx, chat, p.Name)
		switched := a.enterProject(chat, p, was)
		if fresh != "" {
			switched = fresh + "\n" + switched
		}
		return fmt.Sprintf(switched+"\ncreated and attached project %s. Add sources by sending files with the caption /add (or from the dev Mac: make push-project NAME=%s DIR=...). Instructions go in %s/PROJECT.md.", p.Name, p.Name, p.Name)
	default:
		p, err := a.projects.Open(arg)
		if err != nil {
			return err.Error()
		}
		fresh := a.freshFor(ctx, chat, p.Name)
		switched := a.enterProject(chat, p, was)
		rep, err := p.Index.Sync(ctx, nil)
		srcs, _ := p.Index.Sources(ctx)
		var b strings.Builder
		if fresh != "" {
			b.WriteString(fresh + "\n")
		}
		fmt.Fprintf(&b, "project %s attached: %d sources", p.Name, len(srcs))
		if n := len(rep.Added) + len(rep.Updated); n > 0 {
			fmt.Fprintf(&b, " (%d just indexed)", n)
		}
		for name, e := range rep.Failed {
			fmt.Fprintf(&b, "\n  couldn't read %s: %s", name, e)
		}
		if err != nil {
			fmt.Fprintf(&b, "\n  indexing error: %v", err)
		}
		st, _ := p.Settings()
		if st.Cloud {
			b.WriteString("\ncloud profiles can use it too (/project cloud off to stop)")
		} else {
			b.WriteString("\nlocal profiles only (/project cloud on lets cloud profiles use it)")
		}
		b.WriteString("\n" + switched)
		b.WriteString("\n/project off to detach")
		if list := a.projectChats(ctx, chat, p.Name); list != "" {
			b.WriteString("\n\n" + list)
		}
		return b.String()
	}
}

func (a *app) listProjects(ctx context.Context, chat string) string {
	names, err := a.projects.List()
	if err != nil {
		return "error: " + err.Error()
	}
	if len(names) == 0 {
		return "no projects yet: /project new <name>"
	}
	cur := ""
	if p := a.project(chat); p != nil {
		cur = p.Name
	}
	var b strings.Builder
	b.WriteString("projects:\n")
	for _, n := range names {
		mark := "  "
		if n == cur {
			mark = "* "
		}
		detail := ""
		if p, err := a.projects.Open(n); err == nil {
			srcs, _ := p.Index.Sources(ctx)
			detail = fmt.Sprintf(" (%d sources indexed)", len(srcs))
			if st, _ := p.Settings(); st.Cloud {
				detail += ", cloud"
			}
		}
		fmt.Fprintf(&b, "%s%s%s\n", mark, n, detail)
	}
	b.WriteString("/project <name> attaches one to this chat")
	return strings.TrimRight(b.String(), "\n")
}

// addFiles saves attachments to the chat's project. With ask, each file
// needs a yes first (a document sent without /add).
func (a *app) addFiles(ctx context.Context, tr transport.Transport, chat string, files []media.Image, ask bool) string {
	p := a.project(chat)
	if p == nil {
		return "attach a project first (/project <name>, or /project new <name>) to add files to it; images I can also just read"
	}
	if ask {
		var asker approve.Asker
		if ak, ok := tr.(transport.Asker); ok {
			asker = ak
		}
		var names []string
		for _, f := range files {
			names = append(names, fmt.Sprintf("%s (%s)", f.Name, size(len(f.Data))))
		}
		if approve.New(asker, a.cfg.Approval.Timeout).Approve(ctx, chat, fmt.Sprintf("add %s to %s?", strings.Join(names, ", "), p.Name)) != approve.Approved {
			return "not added"
		}
	}
	var saved []string
	for _, f := range files {
		name, err := p.AddSource(f.Name, f.Data)
		if err != nil {
			return "couldn't add " + f.Name + ": " + err.Error()
		}
		saved = append(saved, name)
	}
	ictx, cancel := context.WithTimeout(context.Background(), 30*time.Minute) // OCR of a long scanned manual takes a while
	defer cancel()
	rep, err := p.Index.Sync(ictx, nil)
	if err != nil {
		return "saved, but indexing failed: " + err.Error()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "added to %s: %s", p.Name, strings.Join(saved, ", "))
	srcs, _ := p.Index.Sources(ctx)
	for _, s := range srcs {
		for _, n := range saved {
			if s.Name == n && s.Pages > 1 {
				fmt.Fprintf(&b, "\n  %s: %d pages indexed", n, s.Pages)
			}
		}
	}
	for name, e := range rep.Failed {
		fmt.Fprintf(&b, "\n  couldn't read %s: %s", name, e)
	}
	return b.String()
}

func size(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}
