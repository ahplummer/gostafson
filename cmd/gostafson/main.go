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

	a := &app{cfg: cfg, router: rt, store: store, clients: clients, guards: guards, audit: auditLog,
		showReasoning: showReasoning, log: logger, workers: map[string]chan transport.Message{}}

	var trs []transport.Transport
	var cli *transport.CLI
	if cfg.CLI.On() {
		cli = transport.NewStdio()
		trs = append(trs, cli)
	}
	if cfg.Web.Listen != "" {
		web := transport.NewWeb(cfg.Web.Listen, cfg.Web.ResolvedToken)
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
	}
	return a.serve(ctx, trs)
}

type app struct {
	cfg           *config.Config
	router        *router.Router
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
	if strings.HasPrefix(text, "/") {
		return a.command(ctx, msg.ChatID, text)
	}

	route, err := a.router.Route(text)
	if err != nil {
		return err.Error()
	}
	profile := a.cfg.Profiles[route.Profile]
	if profile.MissingKey != "" {
		return fmt.Sprintf("@%s is unavailable: %s", route.Profile, profile.MissingKey)
	}

	history, err := a.store.Recent(ctx, msg.ChatID, a.cfg.Sessions.MaxMessages)
	if err != nil {
		a.log.Error("load history", "chat", msg.ChatID, "err", err)
		return "error: could not load history"
	}
	if profile.Remote() {
		history = sessions.TextOnly(history) // file contents from tools stay on this machine (PRD D21)
	}

	turnCtx, cancel := context.WithTimeout(ctx, a.cfg.Loop.TurnTimeout)
	defer cancel()
	traceID, spanID := audit.NewTraceID(), audit.NewSpanID()
	var asker approve.Asker
	if ak, ok := tr.(transport.Asker); ok {
		asker = ak
	}
	tools := a.guards[route.Profile].ForTurn(msg.ChatID, traceID, spanID, approve.New(asker, a.cfg.Approval.Timeout))
	start := time.Now()
	res, err := loop.Run(turnCtx, a.clients[route.Profile], tools, loop.Turn{
		Profile:  profile,
		Think:    route.Think,
		History:  history,
		Input:    route.Text,
		MaxSteps: a.cfg.Loop.MaxSteps,
	})
	elapsed := time.Since(start)

	routedBy := "default"
	if route.Tagged {
		routedBy = "tag"
	}
	rec := audit.Turn{
		Time: start.UTC(), Kind: "turn", TraceID: traceID, SpanID: spanID,
		Chat: msg.ChatID, Profile: route.Profile, RoutedBy: routedBy, Model: profile.Model, Think: route.Think,
		Steps: res.Steps, PromptTokens: res.Usage.PromptTokens, CompletionTokens: res.Usage.CompletionTokens,
		DurationMS: elapsed.Milliseconds(),
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
	if err := a.store.Append(ctx, msg.ChatID, res.New...); err != nil {
		a.log.Error("save history", "chat", msg.ChatID, "err", err)
	}

	var b strings.Builder
	if a.showReasoning && res.Reasoning != "" {
		fmt.Fprintf(&b, "[reasoning]\n%s\n[/reasoning]\n\n", strings.TrimSpace(res.Reasoning))
	}
	b.WriteString(strings.TrimSpace(res.Reply))
	if transport.ShowFooter(tr) {
		fmt.Fprintf(&b, "\n[%s | %s | %.1fs | %d tok]", route.Profile, profile.Model, elapsed.Seconds(), res.Usage.TotalTokens)
	}
	return b.String()
}
