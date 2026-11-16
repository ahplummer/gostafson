package sessions

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// Conversations (PRD D38). Messages belong to a conversation; each transport
// chat (cli, web, tg:<id>, wn:<group>) has a current one, and any chat can
// switch to any conversation, so one started on the phone can be picked up
// in the CLI. A conversation keeps its title, sticky profile, attached
// project and a summary of what fell out of the recent window.
//
// History from before conversations existed is one conversation per chat,
// whose ID is the chat ID: messages.chat_id holds the conversation ID.

const conversationSchema = `
CREATE TABLE IF NOT EXISTS conversations (
	id         TEXT PRIMARY KEY,
	title      TEXT NOT NULL DEFAULT '',
	origin     TEXT NOT NULL DEFAULT '', -- the chat it started in
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	profile    TEXT NOT NULL DEFAULT '', -- sticky profile; '' = default
	project    TEXT NOT NULL DEFAULT '',
	summary    TEXT NOT NULL DEFAULT '',
	summarized_through INTEGER NOT NULL DEFAULT 0 -- last message id in the summary
);
CREATE TABLE IF NOT EXISTS current (
	chat_id         TEXT PRIMARY KEY,
	conversation_id TEXT NOT NULL
);
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(text, conversation_id UNINDEXED, tokenize='porter unicode61');
`

// Conversation is one saved conversation.
type Conversation struct {
	ID                string
	Title             string
	Origin            string
	Created, Updated  time.Time
	Profile, Project  string
	Summary           string
	SummarizedThrough int64
	Messages          int
}

// migrate creates the conversation tables and, once, turns each chat's
// existing history into a conversation with the chat's ID.
func (s *Store) migrate() error {
	if _, err := s.db.Exec(conversationSchema); err != nil {
		return err
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM conversations`).Scan(&n)
	if n > 0 {
		return nil
	}
	rows, err := s.db.Query(`SELECT chat_id, MIN(created_at), MAX(created_at) FROM messages GROUP BY chat_id`)
	if err != nil {
		return err
	}
	type old struct{ chat, first, last string }
	var olds []old
	for rows.Next() {
		var o old
		rows.Scan(&o.chat, &o.first, &o.last)
		olds = append(olds, o)
	}
	rows.Close()
	for _, o := range olds {
		if _, err := s.db.Exec(`INSERT INTO conversations (id, title, origin, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
			o.chat, "Earlier chat on "+chatKind(o.chat), o.chat, o.first, o.last); err != nil {
			return err
		}
		if _, err := s.db.Exec(`INSERT OR IGNORE INTO current (chat_id, conversation_id) VALUES (?, ?)`, o.chat, o.chat); err != nil {
			return err
		}
	}
	return s.reindexSearch()
}

// reindexSearch fills the full-text index from the stored messages.
func (s *Store) reindexSearch() error {
	rows, err := s.db.Query(`SELECT chat_id, message FROM messages`)
	if err != nil {
		return err
	}
	type row struct{ conv, raw string }
	var all []row
	for rows.Next() {
		var r row
		rows.Scan(&r.conv, &r.raw)
		all = append(all, r)
	}
	rows.Close()
	for _, r := range all {
		if t := searchText(r.raw); t != "" {
			s.db.Exec(`INSERT INTO messages_fts (text, conversation_id) VALUES (?, ?)`, t, r.conv)
		}
	}
	return nil
}

func chatKind(chat string) string {
	switch {
	case strings.HasPrefix(chat, "tg:"):
		return "Telegram"
	case strings.HasPrefix(chat, "wn:"):
		return "White Noise"
	case chat == "web":
		return "the web page"
	case chat == "cli":
		return "the CLI"
	}
	return chat
}

func newConversationID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return "c-" + hex.EncodeToString(b)
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// Current returns chat's current conversation, starting one if it has none.
func (s *Store) Current(ctx context.Context, chat string) (Conversation, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT conversation_id FROM current WHERE chat_id = ?`, chat).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return s.New(ctx, chat)
	}
	if err != nil {
		return Conversation{}, err
	}
	c, err := s.Get(ctx, id)
	if errors.Is(err, sql.ErrNoRows) { // deleted elsewhere
		return s.New(ctx, chat)
	}
	return c, err
}

// New starts a conversation and makes it chat's current one. The previous
// one is kept.
func (s *Store) New(ctx context.Context, chat string) (Conversation, error) {
	id, t := newConversationID(), now()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO conversations (id, origin, created_at, updated_at) VALUES (?, ?, ?, ?)`, id, chat, t, t); err != nil {
		return Conversation{}, err
	}
	if err := s.Switch(ctx, chat, id); err != nil {
		return Conversation{}, err
	}
	return s.Get(ctx, id)
}

// Switch makes conversation id chat's current one.
func (s *Store) Switch(ctx context.Context, chat, id string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO current (chat_id, conversation_id) VALUES (?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET conversation_id = excluded.conversation_id`, chat, id)
	return err
}

// Get returns one conversation.
func (s *Store) Get(ctx context.Context, id string) (Conversation, error) {
	cs, err := s.query(ctx, `WHERE c.id = ?`, 1, id)
	if err != nil {
		return Conversation{}, err
	}
	if len(cs) == 0 {
		return Conversation{}, sql.ErrNoRows
	}
	return cs[0], nil
}

// List returns the most recently used conversations that have messages,
// newest first. With a query, only those whose messages match it (full-text
// search over what was said).
func (s *Store) List(ctx context.Context, query string, limit int) ([]Conversation, error) {
	terms := ftsTerms(query)
	if len(terms) == 0 {
		return s.query(ctx, `WHERE EXISTS (SELECT 1 FROM messages m WHERE m.chat_id = c.id)`, limit)
	}
	return s.query(ctx, `WHERE c.id IN (SELECT conversation_id FROM messages_fts WHERE messages_fts MATCH ?)`, limit,
		strings.Join(terms, " AND "))
}

// InProject returns the most recently used conversations with messages in
// project, newest first.
func (s *Store) InProject(ctx context.Context, project string, limit int) ([]Conversation, error) {
	return s.query(ctx, `WHERE c.project = ? AND EXISTS (SELECT 1 FROM messages m WHERE m.chat_id = c.id)`, limit, project)
}

func (s *Store) query(ctx context.Context, where string, limit int, args ...any) ([]Conversation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.id, c.title, c.origin, c.created_at, c.updated_at, c.profile, c.project, c.summary,
		c.summarized_through, (SELECT COUNT(*) FROM messages m WHERE m.chat_id = c.id)
		FROM conversations c `+where+` ORDER BY c.updated_at DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Conversation
	for rows.Next() {
		var c Conversation
		var created, updated string
		if err := rows.Scan(&c.ID, &c.Title, &c.Origin, &created, &updated, &c.Profile, &c.Project, &c.Summary, &c.SummarizedThrough, &c.Messages); err != nil {
			return nil, err
		}
		c.Created, _ = time.Parse(time.RFC3339Nano, created)
		c.Updated, _ = time.Parse(time.RFC3339Nano, updated)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ftsTerms quotes each word of a search, so FTS5 syntax in it is just text.
func ftsTerms(q string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(q), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len(w) >= 2 && !searchStop[w] {
			out = append(out, `"`+w+`"`)
		}
	}
	return out
}

// searchStop are words a conversation search ignores: every remaining word
// has to match.
var searchStop = map[string]bool{"or": true, "and": true, "the": true, "of": true, "to": true, "in": true,
	"is": true, "it": true, "an": true, "on": true, "for": true, "my": true, "what": true, "about": true}

// SetTitle names a conversation.
func (s *Store) SetTitle(ctx context.Context, id, title string) error {
	title = strings.TrimSpace(strings.Trim(strings.TrimSpace(title), `"'`))
	if len(title) > 80 {
		title = title[:80]
	}
	_, err := s.db.ExecContext(ctx, `UPDATE conversations SET title = ? WHERE id = ?`, title, id)
	return err
}

// SetState saves a conversation's sticky profile and attached project, so
// they come back after a restart or when the conversation is resumed.
func (s *Store) SetState(ctx context.Context, id, profile, project string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE conversations SET profile = ?, project = ? WHERE id = ?`, profile, project, id)
	return err
}

// SetSummary saves the summary of a conversation's messages up to through.
func (s *Store) SetSummary(ctx context.Context, id, summary string, through int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE conversations SET summary = ?, summarized_through = ? WHERE id = ?`, summary, through, id)
	return err
}

// Delete removes a conversation and its messages for good. Chats whose
// current conversation it was get a new one on their next message.
func (s *Store) Delete(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM messages WHERE chat_id = ?`,
		`DELETE FROM messages_fts WHERE conversation_id = ?`,
		`DELETE FROM current WHERE conversation_id = ?`,
		`DELETE FROM conversations WHERE id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SummaryBatch is how many messages past the recent window wait before
// they are summarized together; until then they are still sent as history.
const SummaryBatch = 6

// Context returns a conversation's history for a turn: the messages not yet
// covered by its summary, at most window+SummaryBatch, so every message is
// either in the summary or sent as itself. Like Recent, it starts at a user
// message.
func (s *Store) Context(ctx context.Context, c Conversation, window int) ([]clientMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT message FROM messages WHERE chat_id = ? AND id > ? ORDER BY id DESC LIMIT ?`,
		c.ID, c.SummarizedThrough, window+SummaryBatch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var msgs []clientMessage
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var m clientMessage
		if err := unmarshalStored(raw, &m); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	for len(msgs) > 0 && msgs[0].Role != "user" {
		msgs = msgs[1:]
	}
	return msgs, nil
}

// Unsummarized returns the messages that have fallen out of the recent
// window (all but the last keep) and aren't in the summary yet, with the
// id of the last one. Like Recent, the cut never splits a turn.
func (s *Store) Unsummarized(ctx context.Context, id string, keep int) ([]MessageWithID, error) {
	c, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, message FROM messages WHERE chat_id = ? AND id > ? AND id <=
		(SELECT COALESCE(MIN(id), 0) - 1 FROM (SELECT id FROM messages WHERE chat_id = ? ORDER BY id DESC LIMIT ?)) ORDER BY id`,
		id, c.SummarizedThrough, id, keep)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MessageWithID
	for rows.Next() {
		var m MessageWithID
		var raw string
		if err := rows.Scan(&m.ID, &raw); err != nil {
			return nil, err
		}
		if err := unmarshalStored(raw, &m.Message); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// TranscriptText renders messages as plain text for a summary or a title:
// text only, no tool results (PRD D21).
func TranscriptText(msgs []MessageWithID) string {
	var plain []clientMessage
	for _, m := range msgs {
		plain = append(plain, m.Message)
	}
	var b strings.Builder
	for _, m := range TextOnly(plain) {
		fmt.Fprintf(&b, "%s: %s\n", m.Role, strings.TrimSpace(m.Content))
	}
	return b.String()
}
