package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Telegram is a bot transport using the Bot API with long polling.
//
// Privacy: Telegram bot chats are not end-to-end encrypted; Telegram's servers
// see every message and reply. Only users listed in allowed_users are
// answered; everyone else is ignored and logged.
type Telegram struct {
	api     string // https://api.telegram.org/bot<token>; never logged
	token   string
	allowed []int64
	http    *http.Client
	log     *slog.Logger

	offset  int64
	updates chan Message
	pending Pending
}

// NewTelegram returns a bot transport. apiBase is normally
// "https://api.telegram.org"; tests pass a fake server.
func NewTelegram(apiBase, token string, allowed []int64, log *slog.Logger) *Telegram {
	return &Telegram{
		api: strings.TrimRight(apiBase, "/") + "/bot" + token, token: token, allowed: allowed,
		http: &http.Client{Timeout: 70 * time.Second}, log: log,
		updates: make(chan Message, 32),
	}
}

// Footer implements Footered: phone chats get the reply without the status line.
func (*Telegram) Footer() bool { return false }

// Name implements Transport.
func (t *Telegram) Name() string { return "telegram" }

const tgPrefix = "tg:"

type tgUpdate struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		From *struct {
			ID int64 `json:"id"`
		} `json:"from"`
		Chat struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		} `json:"chat"`
		Text string `json:"text"`
	} `json:"message"`
	CallbackQuery *struct {
		ID   string `json:"id"`
		From struct {
			ID int64 `json:"id"`
		} `json:"from"`
		Message *struct {
			MessageID int64 `json:"message_id"`
			Chat      struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		} `json:"message"`
		Data string `json:"data"`
	} `json:"callback_query"`
}

// call posts a Bot API method. Errors never include the URL, which holds the token.
func (t *Telegram) call(ctx context.Context, method string, params, result any) error {
	body, _ := json.Marshal(params)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.api+"/"+method, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram %s: bad request", method)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var uerr interface{ Unwrap() error }
		if errors.As(err, &uerr) {
			err = uerr.Unwrap() // drop the *url.Error wrapper and its URL
		}
		return fmt.Errorf("telegram %s: %s", method, strings.ReplaceAll(err.Error(), t.token, "<token>"))
	}
	defer resp.Body.Close()
	var env struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&env); err != nil {
		return fmt.Errorf("telegram %s: HTTP %d", method, resp.StatusCode)
	}
	if !env.OK {
		return fmt.Errorf("telegram %s: %s", method, env.Description)
	}
	if result != nil {
		return json.Unmarshal(env.Result, result)
	}
	return nil
}

// Receive implements Transport. It long-polls getUpdates, drops anything not
// from an allowed user in a private chat, and routes answers to open
// questions (typed or button presses) instead of returning them.
func (t *Telegram) Receive(ctx context.Context) (Message, error) {
	for {
		select {
		case m := <-t.updates:
			return m, nil
		default:
		}
		var ups []tgUpdate
		err := t.call(ctx, "getUpdates", map[string]any{
			"offset": t.offset, "timeout": 50, "allowed_updates": []string{"message", "callback_query"},
		}, &ups)
		if err != nil {
			if ctx.Err() != nil {
				return Message{}, ctx.Err()
			}
			t.log.Warn("telegram poll failed; retrying", "err", err)
			select {
			case <-ctx.Done():
				return Message{}, ctx.Err()
			case <-time.After(5 * time.Second):
			}
			continue
		}
		for _, u := range ups {
			t.offset = u.UpdateID + 1
			t.handleUpdate(ctx, u)
		}
	}
}

func (t *Telegram) handleUpdate(ctx context.Context, u tgUpdate) {
	if cq := u.CallbackQuery; cq != nil {
		t.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": cq.ID}, nil)
		if cq.Message == nil || !slices.Contains(t.allowed, cq.From.ID) {
			return
		}
		chat := tgPrefix + strconv.FormatInt(cq.Message.Chat.ID, 10)
		if t.pending.Deliver(chat, cq.Data) {
			// Replace the buttons with the answer so the question can't be answered twice.
			t.call(ctx, "editMessageReplyMarkup", map[string]any{
				"chat_id": cq.Message.Chat.ID, "message_id": cq.Message.MessageID,
				"reply_markup": map[string]any{"inline_keyboard": [][]map[string]string{{{"text": "→ " + cq.Data, "callback_data": "done"}}}},
			}, nil)
		}
		return
	}
	m := u.Message
	if m == nil || m.From == nil || m.Text == "" {
		return
	}
	if !slices.Contains(t.allowed, m.From.ID) || m.Chat.Type != "private" {
		t.log.Warn("telegram message from a user not in allowed_users; ignored", "user_id", m.From.ID, "chat_type", m.Chat.Type)
		return
	}
	chat := tgPrefix + strconv.FormatInt(m.Chat.ID, 10)
	if t.pending.Deliver(chat, m.Text) {
		return
	}
	t.updates <- Message{ChatID: chat, Sender: strconv.FormatInt(m.From.ID, 10), Text: m.Text}
}

func tgChatID(chatID string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimPrefix(chatID, tgPrefix), 10, 64)
	if err != nil || !strings.HasPrefix(chatID, tgPrefix) {
		return 0, fmt.Errorf("not a telegram chat: %q", chatID)
	}
	return id, nil
}

// tgLimit stays under Telegram's 4096 limit, which counts UTF-16 units
// (an emoji counts twice).
const tgLimit = 4000

// Send implements Transport. Text is split at line breaks when it's longer
// than Telegram allows; each piece is sent as Telegram HTML (bold, code), and
// sent again as plain text if Telegram rejects the formatting.
func (t *Telegram) Send(ctx context.Context, chatID, text string) error {
	id, err := tgChatID(chatID)
	if err != nil {
		return err
	}
	for _, part := range splitText(text, tgLimit) {
		err := t.call(ctx, "sendMessage", map[string]any{"chat_id": id, "text": tgHTML(part), "parse_mode": "HTML"}, nil)
		if err != nil && ctx.Err() == nil {
			t.log.Warn("telegram rejected formatted message; sending plain", "err", err)
			err = t.call(ctx, "sendMessage", map[string]any{"chat_id": id, "text": part}, nil)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

var (
	tgInlineCode = regexp.MustCompile("`([^`\n]+)`")
	tgBold       = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
)

// tgHTML converts the markdown models usually write (code blocks, inline
// code, bold) to Telegram's HTML subset. Everything else is escaped, so model
// output can't inject other tags.
func tgHTML(s string) string {
	var out []string
	var fence []string
	inFence := false
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			if inFence {
				out = append(out, "<pre>"+html.EscapeString(strings.Join(fence, "\n"))+"</pre>")
			}
			inFence, fence = !inFence, nil
			continue
		}
		if inFence {
			fence = append(fence, l)
			continue
		}
		e := html.EscapeString(l)
		e = tgInlineCode.ReplaceAllString(e, "<code>$1</code>")
		e = tgBold.ReplaceAllString(e, "<b>$1</b>")
		out = append(out, e)
	}
	if inFence { // a block cut off by splitText or never closed
		out = append(out, "<pre>"+html.EscapeString(strings.Join(fence, "\n"))+"</pre>")
	}
	return strings.Join(out, "\n")
}

func splitText(s string, limit int) []string {
	r := []rune(s)
	var parts []string
	for len(r) > limit {
		cut := limit
		for i := limit; i > limit/2; i-- { // prefer a line break in the second half
			if r[i] == '\n' {
				cut = i
				break
			}
		}
		parts = append(parts, string(r[:cut]))
		r = r[cut:]
		for len(r) > 0 && r[0] == '\n' {
			r = r[1:]
		}
	}
	return append(parts, string(r))
}

// Busy implements Indicator with Telegram's "typing…" status, refreshed
// every 4 s (it expires after 5).
func (t *Telegram) Busy(chatID string) func() {
	id, err := tgChatID(chatID)
	if err != nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		tick := time.NewTicker(4 * time.Second)
		defer tick.Stop()
		for {
			t.call(ctx, "sendChatAction", map[string]any{"chat_id": id, "action": "typing"}, nil)
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	return cancel
}

// Ask implements Asker: the question comes with Yes/No buttons; a button
// press or the next typed message is the answer.
func (t *Telegram) Ask(ctx context.Context, chatID, question string) (string, error) {
	id, err := tgChatID(chatID)
	if err != nil {
		return "", err
	}
	err = t.call(ctx, "sendMessage", map[string]any{
		"chat_id": id, "text": "❓ " + question,
		"reply_markup": map[string]any{"inline_keyboard": [][]map[string]string{{
			{"text": "Yes", "callback_data": "yes"}, {"text": "No", "callback_data": "no"},
		}}},
	}, nil)
	if err != nil {
		return "", err
	}
	ans, err := t.pending.Wait(ctx, chatID)
	if err != nil {
		t.call(context.Background(), "sendMessage", map[string]any{"chat_id": id, "text": "(no answer: treated as no)"}, nil)
	}
	return ans, err
}
