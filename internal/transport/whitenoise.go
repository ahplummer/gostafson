package transport

import (
	"bufio"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ahplummer/gostafson/internal/media"
)

// WhiteNoise talks to a local wn-agent (the White Noise / Marmot connector)
// over its Unix control socket, protocol marmot.agent-control.v2: one JSON
// object per line, a request ID per request, a new connection per request,
// and one long-lived connection for subscribe_inbound.
//
// wn-agent owns the Marmot account, MLS state and relays; messages are
// end-to-end encrypted between the phone and wn-agent. gostafson answers only
// messages whose sender is in allowed (account IDs as hex); wn-agent's own
// welcomer allowlist decides who can invite the account into a chat.
type WhiteNoise struct {
	socket    string
	authToken string
	account   string // our account_id_hex; found with account_list when empty
	allowed   []string
	log       *slog.Logger

	mu      sync.Mutex // guards subConn, which ctx cancellation reads
	sub     *bufio.Reader
	subConn net.Conn
	subID   string
	pending Pending

	order       sync.Mutex
	notBefore   map[string]time.Time // per chat: earliest send that sorts after the last message
	loggedClock bool
}

const (
	wnProtocol = "marmot.agent-control.v2"
	wnPrefix   = "wn:"
	wnMaxFrame = 1 << 20
)

// White Noise orders a chat by each message's created_at, in whole seconds,
// stamped by the sender's device. A phone clock that runs a second or two
// ahead makes a fast reply sort above the message it answers. So a reply is
// held until its timestamp is a whole second later than the message's. When
// the message carries no timestamp, the reply is held until wnMinGap after
// it arrived. wnMaxHold caps the wait for a clock that is far off.
var (
	wnMinGap  = 3 * time.Second
	wnMaxHold = 5 * time.Second
)

// NewWhiteNoise returns a transport for the wn-agent at socket. allowed holds
// sender account IDs (hex) or npubs.
func NewWhiteNoise(socket, authToken, account string, allowed []string, log *slog.Logger) (*WhiteNoise, error) {
	w := &WhiteNoise{socket: socket, authToken: authToken, account: strings.ToLower(account), log: log}
	for _, a := range allowed {
		h, err := PubkeyHex(a)
		if err != nil {
			return nil, fmt.Errorf("whitenoise.allowed_senders: %w", err)
		}
		w.allowed = append(w.allowed, h)
	}
	if len(w.allowed) == 0 {
		return nil, errors.New("whitenoise.allowed_senders is required")
	}
	return w, nil
}

// Footer implements Footered: phone chats get the reply without the status line.
func (*WhiteNoise) Footer() bool { return false }

// Name implements Transport.
func (w *WhiteNoise) Name() string { return "whitenoise" }

func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (w *WhiteNoise) envelope(id string, payload map[string]any) ([]byte, error) {
	env := map[string]any{"marmot_agent_control": wnProtocol, "id": id}
	for k, v := range payload {
		env[k] = v
	}
	if w.authToken != "" {
		env["auth_token"] = w.authToken
	}
	b, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}
	if len(b)+1 > wnMaxFrame {
		return nil, errors.New("whitenoise: request too large")
	}
	return append(b, '\n'), nil
}

// readFrame reads one envelope and checks protocol, ID and error type.
func readFrame(r *bufio.Reader, id string) (map[string]any, error) {
	line, err := r.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, bufio.ErrBufferFull) {
			return nil, errors.New("whitenoise: frame too large")
		}
		return nil, err
	}
	var env map[string]any
	if err := json.Unmarshal(line, &env); err != nil {
		return nil, errors.New("whitenoise: malformed frame")
	}
	if env["marmot_agent_control"] != wnProtocol {
		return nil, fmt.Errorf("whitenoise: wrong protocol %v", env["marmot_agent_control"])
	}
	if env["id"] != id {
		return nil, errors.New("whitenoise: response id mismatch")
	}
	if env["type"] == "error" {
		return nil, fmt.Errorf("whitenoise: %v (%v)", env["message"], env["code"])
	}
	return env, nil
}

// request sends one request on a fresh connection and returns the response.
func (w *WhiteNoise) request(ctx context.Context, payload map[string]any) (map[string]any, error) {
	limit := 30 * time.Second
	if payload["type"] == "download_media" {
		limit = 2 * time.Minute // fetches and decrypts the file
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", w.socket)
	if err != nil {
		return nil, fmt.Errorf("whitenoise: %w", err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	id := newID()
	frame, err := w.envelope(id, payload)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(frame); err != nil {
		return nil, fmt.Errorf("whitenoise: %w", err)
	}
	return readFrame(bufio.NewReaderSize(conn, wnMaxFrame), id)
}

// resolveAccount picks our account: the configured one, or the only local
// signing account wn-agent has.
func (w *WhiteNoise) resolveAccount(ctx context.Context) error {
	if w.account != "" {
		return nil
	}
	resp, err := w.request(ctx, map[string]any{"type": "account_list"})
	if err != nil {
		return err
	}
	accounts, _ := resp["accounts"].([]any)
	var ids []string
	for _, a := range accounts {
		m, _ := a.(map[string]any)
		if id, ok := m["account_id_hex"].(string); ok && m["local_signing"] == true {
			ids = append(ids, strings.ToLower(id))
		}
	}
	if len(ids) != 1 {
		return fmt.Errorf("whitenoise: wn-agent has %d signing accounts; set whitenoise.account_id_hex", len(ids))
	}
	w.account = ids[0]
	return nil
}

func (w *WhiteNoise) subscribe(ctx context.Context) error {
	if err := w.resolveAccount(ctx); err != nil {
		return err
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", w.socket)
	if err != nil {
		return fmt.Errorf("whitenoise: %w", err)
	}
	id := newID()
	frame, _ := w.envelope(id, map[string]any{"type": "subscribe_inbound", "account_id_hex": w.account, "group_id_hex": nil})
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	r := bufio.NewReaderSize(conn, wnMaxFrame)
	if _, err := conn.Write(frame); err != nil {
		conn.Close()
		return fmt.Errorf("whitenoise: %w", err)
	}
	ack, err := readFrame(r, id)
	if err != nil {
		conn.Close()
		return err
	}
	if ack["type"] != "ack" {
		conn.Close()
		return fmt.Errorf("whitenoise: expected subscribe ack, got %v", ack["type"])
	}
	conn.SetDeadline(time.Time{})
	w.mu.Lock()
	w.subConn, w.sub, w.subID = conn, r, id
	w.mu.Unlock()
	return nil
}

func (w *WhiteNoise) closeSub() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.subConn != nil {
		w.subConn.Close()
	}
	w.subConn, w.sub = nil, nil
}

// Receive implements Transport. It keeps the inbound subscription open,
// reconnecting with a delay on failure, and returns messages from allowed
// senders. A message in a chat with an open question answers it instead.
func (w *WhiteNoise) Receive(ctx context.Context) (Message, error) {
	stop := context.AfterFunc(ctx, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.subConn != nil {
			w.subConn.SetReadDeadline(time.Now()) // unblock the read
		}
	})
	defer stop()
	for {
		if ctx.Err() != nil {
			w.closeSub()
			return Message{}, ctx.Err()
		}
		if w.sub == nil {
			if err := w.subscribe(ctx); err != nil {
				if ctx.Err() != nil {
					return Message{}, ctx.Err()
				}
				w.log.Warn("whitenoise subscribe failed; retrying", "err", err)
				select {
				case <-ctx.Done():
					return Message{}, ctx.Err()
				case <-time.After(5 * time.Second):
				}
				continue
			}
		}
		ev, err := readFrame(w.sub, w.subID)
		if err != nil {
			w.closeSub()
			if ctx.Err() == nil {
				w.log.Warn("whitenoise subscription ended; reconnecting", "err", err)
			}
			continue
		}
		switch ev["type"] {
		case "resync_required":
			w.log.Warn("whitenoise connector asked for a resync; reconnecting")
			w.closeSub()
			continue
		case "inbound_message":
		default:
			continue
		}
		m, ok := w.parse(ev)
		if !ok {
			continue
		}
		w.noteArrival(m.ChatID, ev)
		w.fetchImages(ctx, ev, &m)
		if len(m.Images)+len(m.Files) == 0 && strings.TrimSpace(m.Text) == "" {
			continue // only unsupported attachments, already answered
		}
		if len(m.Images)+len(m.Files) == 0 && w.pending.Deliver(m.ChatID, m.Text) {
			continue
		}
		return m, nil
	}
}

func (w *WhiteNoise) parse(ev map[string]any) (Message, bool) {
	group, _ := ev["group_id_hex"].(string)
	msg, _ := ev["message"].(map[string]any)
	sender, _ := msg["sender"].(map[string]any)
	from, _ := sender["account_id_hex"].(string)
	text, _ := msg["text"].(string)
	from = strings.ToLower(from)
	refs, _ := msg["media"].([]any)
	if group == "" || from == "" || (strings.TrimSpace(text) == "" && len(refs) == 0) || from == w.account {
		return Message{}, false
	}
	if !slices.Contains(w.allowed, from) {
		w.log.Warn("whitenoise message from a sender not in allowed_senders; ignored", "sender", from)
		return Message{}, false
	}
	return Message{ChatID: wnPrefix + strings.ToLower(group), Sender: from, Text: text}, true
}

// noteArrival records when a reply in chat may be sent (see wnMinGap).
func (w *WhiteNoise) noteArrival(chat string, ev map[string]any) {
	now := time.Now()
	at, field := createdAt(ev)
	until := now.Add(wnMinGap)
	if !at.IsZero() {
		until = time.Unix(at.Unix()+1, 0) // the next whole second after the message's
	}
	if limit := now.Add(wnMaxHold); until.After(limit) {
		until = limit
	}
	w.order.Lock()
	defer w.order.Unlock()
	if w.notBefore == nil {
		w.notBefore = map[string]time.Time{}
	}
	w.notBefore[chat] = until
	if !w.loggedClock {
		w.loggedClock = true
		if at.IsZero() {
			msg, _ := ev["message"].(map[string]any)
			w.log.Warn("whitenoise message has no timestamp; replies wait "+wnMinGap.String(), "message_fields", slices.Sorted(maps.Keys(msg)))
		} else {
			w.log.Warn("whitenoise sender clock (logged once)", "field", field, "ahead_by", at.Sub(now).Round(100*time.Millisecond))
		}
	}
}

// createdAt finds the message's timestamp in an inbound event, in seconds or
// milliseconds since 1970, and names the field it came from.
func createdAt(ev map[string]any) (time.Time, string) {
	msg, _ := ev["message"].(map[string]any)
	for _, src := range []struct {
		m   map[string]any
		pre string
	}{{msg, "message."}, {ev, ""}} {
		for _, k := range []string{"created_at", "created_at_unix", "timestamp", "sent_at"} {
			var n float64
			switch v := src.m[k].(type) {
			case float64:
				n = v
			case string:
				f, err := strconv.ParseFloat(v, 64)
				if err != nil {
					continue
				}
				n = f
			default:
				continue
			}
			if n > 1e12 {
				n /= 1000 // milliseconds
			}
			if n > 1e9 {
				return time.Unix(int64(n), 0), src.pre + k
			}
		}
	}
	return time.Time{}, ""
}

// hold waits until a reply in chat sorts after the message it answers.
func (w *WhiteNoise) hold(ctx context.Context, chat string) error {
	w.order.Lock()
	until := w.notBefore[chat]
	w.order.Unlock()
	d := time.Until(until)
	if d <= 0 {
		return nil
	}
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// fetchImages downloads a message's attachments through wn-agent
// (download_media writes the decrypted file into its home, which this user
// can read) and deletes wn-agent's copy. Images go to Images; PDFs and text
// files to Files (for a project); anything else gets a short reply.
func (w *WhiteNoise) fetchImages(ctx context.Context, ev map[string]any, m *Message) {
	msg, _ := ev["message"].(map[string]any)
	refs, _ := msg["media"].([]any)
	group := strings.TrimPrefix(m.ChatID, wnPrefix)
	var skipped []string
	for _, r := range refs {
		ref, ok := r.(map[string]any)
		if !ok {
			continue
		}
		mt, _ := ref["media_type"].(string)
		name, _ := ref["file_name"].(string)
		isImage := mt == "" || strings.HasPrefix(mt, "image/")
		if !isImage && !isDocument(mt, name) {
			skipped = append(skipped, cmp.Or(name, mt))
			continue
		}
		data, err := w.downloadMedia(ctx, group, ref)
		if err != nil {
			w.log.Warn("whitenoise download failed", "err", err)
			skipped = append(skipped, cmp.Or(name, "a file")+" (download failed)")
			continue
		}
		if isImage {
			m.Images = append(m.Images, media.Image{Name: name, Data: data})
		} else {
			m.Files = append(m.Files, media.Image{Name: name, Data: data})
		}
	}
	if len(skipped) > 0 {
		w.Send(ctx, m.ChatID, "I can read images, PDFs and text files; skipped: "+strings.Join(skipped, ", "))
	}
}

// isDocument reports a PDF or text attachment, by media type or file name.
func isDocument(mt, name string) bool {
	switch mt {
	case "application/pdf", "text/plain", "text/markdown", "text/csv":
		return true
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".pdf", ".txt", ".md", ".markdown", ".csv":
		return true
	}
	return false
}

func (w *WhiteNoise) downloadMedia(ctx context.Context, group string, ref map[string]any) ([]byte, error) {
	resp, err := w.request(ctx, map[string]any{
		"type": "download_media", "account_id_hex": w.account, "group_id_hex": group, "media": ref,
	})
	if err != nil {
		return nil, err
	}
	path, _ := resp["path"].(string)
	if path == "" {
		return nil, errors.New("download_media returned no path")
	}
	defer os.Remove(path) // wn-agent's decrypted copy; the image now lives in gostafson's store
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > media.MaxBytes {
		return nil, fmt.Errorf("the image is %d MB; the limit is %d MB", info.Size()>>20, media.MaxBytes>>20)
	}
	return os.ReadFile(path)
}

// Send implements Transport.
func (w *WhiteNoise) Send(ctx context.Context, chatID, text string) error {
	group, ok := strings.CutPrefix(chatID, wnPrefix)
	if !ok {
		return fmt.Errorf("not a whitenoise chat: %q", chatID)
	}
	if err := w.hold(ctx, chatID); err != nil {
		return err
	}
	if err := w.resolveAccount(ctx); err != nil {
		return err
	}
	_, err := w.request(ctx, map[string]any{
		"type": "send_final", "account_id_hex": w.account, "group_id_hex": group,
		"text": text, "reply_to_message_id_hex": nil,
		"idempotency_key": newID(), // lets wn-agent drop a duplicate if a send is retried
	})
	return err
}

// Ask implements Asker: the question is a message; the chat's next message
// is the answer.
func (w *WhiteNoise) Ask(ctx context.Context, chatID, question string) (string, error) {
	if err := w.Send(ctx, chatID, "❓ "+question+"\n(reply yes or no)"); err != nil {
		return "", err
	}
	ans, err := w.pending.Wait(ctx, chatID)
	if err != nil {
		w.Send(context.Background(), chatID, "(no answer: treated as no)")
	}
	return ans, err
}

// PubkeyHex accepts a Nostr public key as 64 hex characters or an npub and
// returns lowercase hex.
func PubkeyHex(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if strings.HasPrefix(s, "npub1") {
		data, err := bech32Decode(s, "npub")
		if err != nil {
			return "", fmt.Errorf("%q: %w", s, err)
		}
		if len(data) != 32 {
			return "", fmt.Errorf("%q: decoded to %d bytes, want 32", s, len(data))
		}
		return hex.EncodeToString(data), nil
	}
	if b, err := hex.DecodeString(s); err != nil || len(b) != 32 {
		return "", fmt.Errorf("%q: want an npub or 64 hex characters", s)
	}
	return s, nil
}

const bech32Chars = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

// bech32Decode decodes a bech32 string (BIP-173) with the given prefix and
// returns the data converted from 5-bit groups to bytes.
func bech32Decode(s, hrp string) ([]byte, error) {
	sep := strings.LastIndexByte(s, '1')
	if sep < 1 || s[:sep] != hrp || len(s)-sep-1 < 6 {
		return nil, errors.New("not a valid " + hrp)
	}
	var values []byte
	for _, c := range s[sep+1:] {
		i := strings.IndexRune(bech32Chars, c)
		if i < 0 {
			return nil, errors.New("invalid bech32 character")
		}
		values = append(values, byte(i))
	}
	if bech32Polymod(append(bech32HRPExpand(hrp), values...)) != 1 {
		return nil, errors.New("bad checksum")
	}
	values = values[:len(values)-6]
	var out []byte
	acc, bits := 0, 0
	for _, v := range values {
		acc = acc<<5 | int(v)
		bits += 5
		for bits >= 8 {
			bits -= 8
			out = append(out, byte(acc>>bits))
		}
		acc &= 1<<bits - 1
	}
	if bits >= 5 || acc != 0 {
		return nil, errors.New("invalid padding")
	}
	return out, nil
}

func bech32HRPExpand(hrp string) []byte {
	var out []byte
	for _, c := range hrp {
		out = append(out, byte(c>>5))
	}
	out = append(out, 0)
	for _, c := range hrp {
		out = append(out, byte(c&31))
	}
	return out
}

func bech32Polymod(values []byte) int {
	gen := []int{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	chk := 1
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ int(v)
		for i := 0; i < 5; i++ {
			if (top>>i)&1 == 1 {
				chk ^= gen[i]
			}
		}
	}
	return chk
}
