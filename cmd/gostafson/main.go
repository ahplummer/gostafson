// Command gostafson is a small, local-first agent harness.
// See docs/reference-architecture.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ahplummer/gostafson/internal/approve"
	"github.com/ahplummer/gostafson/internal/audit"
	"github.com/ahplummer/gostafson/internal/client"
	"github.com/ahplummer/gostafson/internal/config"
	"github.com/ahplummer/gostafson/internal/guard"
	"github.com/ahplummer/gostafson/internal/loop"
	"github.com/ahplummer/gostafson/internal/media"
	"github.com/ahplummer/gostafson/internal/projects"
	"github.com/ahplummer/gostafson/internal/router"
	"github.com/ahplummer/gostafson/internal/sessions"
	"github.com/ahplummer/gostafson/internal/transport"
)

var version = "dev" // set by -ldflags at build time

func main() {
	configPath := flag.String("config", "config.toml", "path to the config file")
	showReasoning := flag.Bool("show-reasoning", false, "include the model's reasoning in replies when think=true")
	showVersion := flag.Bool("version", false, "print the version and exit")
	verbose := flag.Bool("v", false, "log startup and a line per turn to stderr (always in the audit log)")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}
	level := slog.LevelWarn // warnings and errors only, so the CLI shows just replies
	if *verbose {
		level = slog.LevelInfo
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	if err := run(*configPath, *showReasoning, logger); err != nil {
		logger.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func run(configPath string, showReasoning bool, logger *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	rt, err := router.New(cfg.ProfileNames(), cfg.DefaultProfile)
	if err != nil {
		return err
	}
	store, err := sessions.Open(cfg.Sessions.Path)
	if err != nil {
		return err
	}
	defer store.Close()

	auditLog, err := audit.Open(cfg.Audit.Path)
	if err != nil {
		return err
	}
	defer auditLog.Close()

	images, err := media.OpenStore(filepath.Join(filepath.Dir(cfg.Sessions.Path), "images"))
	if err != nil {
		return err
	}

	var embedder projects.Embedder
	if m := cfg.Projects.EmbedModel; m != "none" {
		embedder = &projects.OllamaEmbedder{Client: client.New(cfg.Profiles[cfg.DefaultProfile].BaseURL, ""), Model: m}
	}
	extractor := &projects.Extractor{CacheDir: filepath.Join(cfg.Projects.Dir, ".cache")}
	projectStore, err := projects.NewStore(cfg.Projects.Dir, extractor, embedder)
	if err != nil {
		return err
	}
	defer projectStore.Close()

	clients := make(map[string]*client.Client, len(cfg.Profiles))
	guards := make(map[string]*guard.Guard, len(cfg.Profiles))
	for name, p := range cfg.Profiles {
		clients[name] = client.New(p.BaseURL, p.ResolvedAPIKey)
		g, err := guard.New(name, p.Tools, p.Root, auditLog)
		if err != nil {
			return err
		}
		defer g.Close()
		guards[name] = g
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sticky := router.NewSticky(rt, func(p string) bool { return cfg.Profiles[p].Sticks() })
	models := map[string]string{}
	for name, p := range cfg.Profiles {
		models[name] = p.Model
	}
	a := &app{cfg: cfg, router: rt, sticky: sticky, images: images, vision: map[string]bool{}, models: models, store: store, clients: clients, guards: guards, audit: auditLog,
		showReasoning: showReasoning, log: logger, workers: map[string]chan transport.Message{},
		projects: projectStore, attached: map[string]string{}, loaded: map[string]string{}, listing: map[string][]string{}}
	extractor.Describe = a.describeImage

	var trs []transport.Transport
	var cli *transport.CLI
	if cfg.CLI.On() {
		cli = transport.NewStdio()
		words := []string{"/help", "/new", "/chats", "/chat", "/title", "/delete", "/profiles", "/projects", "/project", "/add", "/status", "/reset", "/exit"}
		for _, t := range rt.Tags() {
			words = append(words, "@"+t)
		}
		cli.Complete = func(string) []string { return words }
		cli.Status = func() string { // shown before the prompt: "sonnet · travato ❯"
			var parts []string
			if p := a.sticky.Active(cli.ChatID); p != cfg.DefaultProfile {
				parts = append(parts, p)
			}
			if p := a.project(cli.ChatID); p != nil {
				parts = append(parts, p.Name)
			}
			return strings.Join(parts, " · ")
		}
		trs = append(trs, cli)
	}
	if cfg.Web.Listen != "" {
		web := transport.NewWeb(cfg.Web.Listen, cfg.Web.ResolvedToken)
		web.Conversations = a.webConversations
		web.History = a.webHistory
		if err := web.Start(ctx, logger); err != nil {
			return err
		}
		trs = append(trs, web)
		logger.Info("web page", "url", "http://"+cfg.Web.Listen)
	}
	if cfg.Telegram.On() {
		trs = append(trs, transport.NewTelegram(cfg.Telegram.APIBase, cfg.Telegram.ResolvedToken, cfg.Telegram.AllowedUsers, logger))
		logger.Info("telegram bot", "allowed_users", cfg.Telegram.AllowedUsers)
	}
	if wn := cfg.WhiteNoise; wn.Socket != "" {
		t, err := transport.NewWhiteNoise(wn.Socket, wn.ResolvedAuthToken, wn.AccountIDHex, wn.AllowedSenders, logger)
		if err != nil {
			return err
		}
		trs = append(trs, t)
		logger.Info("whitenoise", "socket", wn.Socket)
	}
	if len(trs) == 0 {
		return errors.New("no transport enabled: set web.listen, [telegram] or [whitenoise], or leave cli.enabled on")
	}
	a.transports = trs
	for _, name := range cfg.ProfileNames() {
		if p := cfg.Profiles[name]; p.MissingKey != "" {
			logger.Warn("profile unavailable", "profile", name, "reason", p.MissingKey)
		}
	}
	logger.Info("started", "version", version, "profiles", cfg.ProfileNames(), "default", cfg.DefaultProfile)
	if cli != nil {
		cli.Banner(version)
		if cfg.Web.Listen != "" {
			cli.Note("web page: http://" + cfg.Web.Listen)
		}
		cli.Note("Tab completes / and @ · ↑ history · Ctrl+V (or ⌘V) pastes a screenshot · drag in an image file")
	}
	return a.serve(ctx, trs)
}

type app struct {
	cfg           *config.Config
	router        *router.Router
	sticky        *router.Sticky    // per-chat profile from the last sticky tag (PRD D32, D33)
	images        *media.Store      // each chat's current images (PRD D34)
	models        map[string]string // profile -> model, for labeling replies (PRD D36)
	projects      *projects.Store
	projMu        sync.Mutex
	attached      map[string]string   // chat -> attached project (PRD D37)
	loaded        map[string]string   // chat -> conversation whose profile and project are in sticky/attached (PRD D38)
	listing       map[string][]string // chat -> conversation ids of its last /chats, for /chat N
	bgMu          sync.Mutex          // one background title or summary at a time
	visionMu      sync.Mutex
	vision        map[string]bool // profile -> takes images, filled on first use
	store         *sessions.Store
	clients       map[string]*client.Client
	guards        map[string]*guard.Guard
	audit         *audit.Log
	transports    []transport.Transport
	showReasoning bool
	log           *slog.Logger

	mu      sync.Mutex
	workers map[string]chan transport.Message // per-chat queues for non-sequential transports
	wg      sync.WaitGroup
}

// handle processes one message and returns the reply text.
// Approvals go back over tr, the transport the message came from.
func (a *app) handle(ctx context.Context, tr transport.Transport, msg transport.Message) string {
	text := strings.TrimSpace(msg.Text)
	conv, err := a.conversation(ctx, msg.ChatID)
	if err != nil {
		a.log.Error("open conversation", "chat", msg.ChatID, "err", err)
		return "error: could not open the conversation"
	}
	if (text == "/add" || strings.HasPrefix(text, "/add ")) && len(msg.Images)+len(msg.Files) > 0 {
		return a.addFiles(ctx, tr, msg.ChatID, append(msg.Images, msg.Files...), false)
	}
	added := ""
	if len(msg.Files) > 0 { // a document: it can only be a project source
		added = a.addFiles(ctx, tr, msg.ChatID, msg.Files, true)
		if text == "" && len(msg.Images) == 0 {
			return added
		}
		added += "\n\n"
	}
	if strings.HasPrefix(text, "/") && len(msg.Images) == 0 {
		return added + a.command(ctx, msg.ChatID, text)
	}
	if len(msg.Images) > 0 && text == "" {
		text = imagePrompt
	}

	route, err := a.sticky.Route(msg.ChatID, text)
	a.saveState(ctx, msg.ChatID, conv.ID)                                       // a tag may have changed the sticky profile
	if errors.Is(err, router.ErrEmpty) && route.Tagged && len(msg.Images) > 0 { // "@code" + a photo
		route.Text, err = imagePrompt, nil
	}
	if errors.Is(err, router.ErrEmpty) && route.Tagged { // just a tag: switch the chat
		p := a.cfg.Profiles[route.Profile]
		switch {
		case !p.Sticks():
			return fmt.Sprintf("@%s is a cloud profile: tag each message you want sent there (this chat stays on @%s)", route.Profile, a.sticky.Active(msg.ChatID))
		case p.Remote():
			return fmt.Sprintf("this chat now sends untagged messages to @%s (%s, off this machine) until another tag, @%s or /reset; replies start with [%s]",
				route.Profile, host(p.BaseURL), a.cfg.DefaultProfile, route.Profile)
		}
		if route.Profile == a.cfg.DefaultProfile {
			return fmt.Sprintf("this chat is back on @%s", route.Profile)
		}
		return fmt.Sprintf("this chat now uses @%s until another tag, @%s or /reset", route.Profile, a.cfg.DefaultProfile)
	}
	if err != nil {
		return err.Error()
	}
	profile := a.cfg.Profiles[route.Profile]
	if profile.MissingKey != "" {
		return fmt.Sprintf("@%s is unavailable: %s", route.Profile, profile.MissingKey)
	}

	var ims []media.Image
	var refs []string
	if len(msg.Images) > 0 {
		if len(msg.Images) > media.MaxImages {
			return fmt.Sprintf("up to %d images per message, please", media.MaxImages)
		}
		if !a.canSee(ctx, route.Profile) {
			return fmt.Sprintf("@%s (%s) can't read images; send it with a tag for a profile that can, such as @%s", route.Profile, profile.Model, a.cfg.DefaultProfile)
		}
		for _, raw := range msg.Images {
			im, err := media.Prepare(ctx, raw.Name, raw.Data)
			if err != nil {
				return "can't use that image: " + err.Error()
			}
			ims = append(ims, im)
		}
		if refs, err = a.images.SetCurrent(conv.ID, ims); err != nil {
			a.log.Error("save images", "chat", msg.ChatID, "err", err)
			return "error: could not save the image"
		}
	}

	history, err := a.store.Context(ctx, conv, a.cfg.Sessions.MaxMessages)
	if err != nil {
		a.log.Error("load history", "chat", msg.ChatID, "err", err)
		return "error: could not load history"
	}
	if profile.Remote() {
		history = sessions.TextOnly(history) // file contents from tools stay on this machine (PRD D21)
	}
	history = a.historyImages(ctx, conv.ID, route.Profile, history)
	history = sessions.LabelOthers(history, route.Profile, a.models)

	// The attached project: its context in the system prompt, its tools for
	// this turn. Cloud profiles get it only with cloud = true (PRD D37).
	runProfile := profile
	var proj *projects.Project
	if p := a.project(msg.ChatID); p != nil {
		st, _ := p.Settings()
		if profile.Remote() && !st.Cloud {
			added += fmt.Sprintf("[project %s isn't shared with cloud profiles; /project cloud on allows it]\n", p.Name)
		} else {
			proj = p
			if rep, err := p.Index.Sync(ctx, nil); err == nil && len(rep.Failed) > 0 {
				a.log.Warn("project sources unreadable", "project", p.Name, "failed", rep.Failed)
			}
			pp := *profile
			pp.SystemPrompt = strings.TrimSpace(pp.SystemPrompt + "\n\n" + p.Context(ctx, true) + p.Passages(ctx, route.Text))
			runProfile = &pp
		}
	}

	if conv.Summary != "" { // what fell out of the recent window (PRD D38)
		pp := *runProfile
		pp.SystemPrompt = strings.TrimSpace(pp.SystemPrompt + "\n\n## Earlier in this conversation\n" +
			"(a summary of older messages; the recent ones follow)\n" + conv.Summary)
		runProfile = &pp
	}

	turnCtx, cancel := context.WithTimeout(ctx, a.cfg.Loop.TurnTimeout)
	defer cancel()
	traceID, spanID := audit.NewTraceID(), audit.NewSpanID()
	var asker approve.Asker
	if ak, ok := tr.(transport.Asker); ok {
		asker = ak
	}
	tools := a.guards[route.Profile].ForTurn(msg.ChatID, traceID, spanID, approve.New(asker, a.cfg.Approval.Timeout))
	if proj != nil {
		tools = tools.With(proj.Tools())
	}
	start := time.Now()
	res, err := loop.Run(turnCtx, a.clients[route.Profile], tools, loop.Turn{
		Profile:   runProfile,
		Think:     route.Think,
		History:   history,
		Input:     route.Text,
		Images:    ims,
		ImageRefs: refs,
		MaxSteps:  a.cfg.Loop.MaxSteps,
	})
	elapsed := time.Since(start)

	routedBy := "default"
	switch {
	case route.Tagged:
		routedBy = "tag"
	case route.Sticky:
		routedBy = "sticky"
	}
	rec := audit.Turn{
		Time: start.UTC(), Kind: "turn", TraceID: traceID, SpanID: spanID,
		Chat: msg.ChatID, Profile: route.Profile, RoutedBy: routedBy, Model: profile.Model, Think: route.Think,
		Steps: res.Steps, PromptTokens: res.Usage.PromptTokens, CompletionTokens: res.Usage.CompletionTokens,
		DurationMS: elapsed.Milliseconds(),
	}
	if proj != nil {
		rec.Project = proj.Name
	}
	for _, im := range ims {
		rec.Images = append(rec.Images, audit.Image{Name: im.Name, MediaType: im.MediaType, Bytes: len(im.Data), SHA256: im.SHA256()})
	}
	if err != nil {
		rec.Error = err.Error()
	}
	if werr := a.audit.Write(rec); werr != nil {
		a.log.Error("audit write", "err", werr)
	}
	a.log.Info("turn", "trace", traceID, "chat", msg.ChatID, "profile", route.Profile, "routed_by", routedBy, "model", profile.Model,
		"think", route.Think, "steps", res.Steps, "tokens", res.Usage.TotalTokens, "duration", elapsed.Round(time.Millisecond), "err", err)

	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Sprintf("error: turn timed out after %s", a.cfg.Loop.TurnTimeout)
		}
		return "error: " + err.Error()
	}
	if err := a.store.Append(ctx, conv.ID, res.New...); err != nil {
		a.log.Error("save history", "chat", msg.ChatID, "err", err)
	}
	go a.afterTurn(conv.ID) // title and summary, in the background

	var b strings.Builder
	b.WriteString(added)
	if a.showReasoning && res.Reasoning != "" {
		fmt.Fprintf(&b, "[reasoning]\n%s\n[/reasoning]\n\n", strings.TrimSpace(res.Reasoning))
	}
	// Say which profile answered, and the attached project, when the footer
	// won't (phones); and always while a sticky cloud profile is in use, so
	// it's clear messages leave this machine.
	who := route.Profile
	attached := a.project(msg.ChatID)
	if attached != nil {
		who += " · " + attached.Name
	}
	stickyCloud := profile.Remote() && profile.Sticky && (route.Sticky || route.Tagged)
	if stickyCloud || (!transport.ShowFooter(tr) && (route.Sticky || attached != nil)) {
		fmt.Fprintf(&b, "[%s] ", who)
	}
	b.WriteString(strings.TrimSpace(res.Reply))
	if transport.ShowFooter(tr) {
		fmt.Fprintf(&b, "\n[%s | %s | %.1fs | %d tok]", who, profile.Model, elapsed.Seconds(), res.Usage.TotalTokens)
	}
	return b.String()
}

// imagePrompt stands in for the text of a message that is only an image.
const imagePrompt = "What is in this image?"

// canSee reports whether a profile's model takes images, asking its server
// once. When the server can't say, the answer is yes: the model's own error
// is clearer than refusing an image it might read.
func (a *app) canSee(ctx context.Context, profile string) bool {
	a.visionMu.Lock()
	ok, known := a.vision[profile]
	a.visionMu.Unlock()
	if known {
		return ok
	}
	ok, err := a.clients[profile].Vision(ctx, a.cfg.Profiles[profile].Model)
	if err != nil {
		a.log.Info("vision check failed; assuming images work", "profile", profile, "err", err)
		return true
	}
	a.visionMu.Lock()
	a.vision[profile] = ok
	a.visionMu.Unlock()
	return ok
}

// historyImages re-attaches the chat's current image to the message that
// sent it, so follow-ups ("what color is the car?") can still see it. Older
// images, and every image for a remote profile, become a short note: an
// image reaches a cloud model only with the message it was sent with (PRD D34).
func (a *app) historyImages(ctx context.Context, chat, profile string, history []client.Message) []client.Message {
	remote := a.cfg.Profiles[profile].Remote()
	see := !remote && a.canSee(ctx, profile)
	for i, m := range history {
		if len(m.ImageRefs) == 0 {
			continue
		}
		if see {
			if ims := a.images.Current(chat, m.ImageRefs); len(ims) > 0 {
				history[i].Images = ims
				continue
			}
		}
		note := "[an earlier image; it isn't attached any more]"
		if remote {
			note = "[an image was shared here; the model that answered saw it, but it isn't sent to you]"
		}
		history[i].Content = strings.TrimSpace(m.Content + "\n" + note)
	}
	return history
}
