// Package sessions stores conversation history per chat in SQLite.
package sessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ahplummer/gostafson/internal/client"
	_ "modernc.org/sqlite" // registers the "sqlite" driver (pure Go, no cgo)
)

const schema = `
CREATE TABLE IF NOT EXISTS messages (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	chat_id    TEXT    NOT NULL,
	created_at TEXT    NOT NULL,
	message    TEXT    NOT NULL -- client.Message as JSON
);
CREATE INDEX IF NOT EXISTS messages_chat ON messages(chat_id, id);
`

// Store is a SQLite-backed history store.
type Store struct {
	db *sql.DB
}

// Open opens or creates the database at path. The parent directory is
// created with mode 0700 if missing.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("sessions dir: %w", err)
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("sessions schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Append adds messages to a chat's history in one transaction.
func (s *Store) Append(ctx context.Context, chatID string, msgs ...client.Message) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, m := range msgs {
		b, err := json.Marshal(stored(m))
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO messages (chat_id, created_at, message) VALUES (?, ?, ?)`, chatID, now, string(b)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Recent returns up to limit of the chat's most recent messages, oldest first.
// The result always starts at a user message, so a cut never leaves a tool
// result or tool call without the turn that started it.
func (s *Store) Recent(ctx context.Context, chatID string, limit int) ([]client.Message, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT message FROM messages WHERE chat_id = ? ORDER BY id DESC LIMIT ?`, chatID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []client.Message
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var m storedMessage
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return nil, err
		}
		msgs = append(msgs, client.Message(m))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Reverse to oldest first.
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	for len(msgs) > 0 && msgs[0].Role != "user" {
		msgs = msgs[1:]
	}
	return msgs, nil
}

// storedMessage is a message as saved: the wire fields plus image refs (the
// image data itself is never stored here). Its plain JSON encoding skips the
// wire form's MarshalJSON.
type storedMessage client.Message

func stored(m client.Message) storedMessage {
	m.Images = nil
	return storedMessage(m)
}

// Count returns the number of stored messages for a chat.
func (s *Store) Count(ctx context.Context, chatID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE chat_id = ?`, chatID).Scan(&n)
	return n, err
}

// Reset deletes a chat's history.
func (s *Store) Reset(ctx context.Context, chatID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM messages WHERE chat_id = ?`, chatID)
	return err
}

// TextOnly returns history for profiles on a remote server: tool results
// (file contents a tool read on this machine) are removed (PRD D21). Each tool
// call becomes a short note in the assistant's text, naming the tool and its
// path argument only, so the remote model can see that a local tool ran and
// that its output was withheld, rather than concluding earlier replies were
// made up. Consecutive assistant messages are merged.
func TextOnly(msgs []client.Message) []client.Message {
	out := make([]client.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == "tool" {
			continue
		}
		if m.Role == "assistant" {
			text := m.Content
			for _, c := range m.ToolCalls {
				text = joinText(text, toolNote(c))
			}
			if text == "" {
				continue
			}
			if n := len(out); n > 0 && out[n-1].Role == "assistant" && out[n-1].Profile == m.Profile {
				out[n-1].Content = joinText(out[n-1].Content, text)
				continue
			}
			m = client.Message{Role: "assistant", Content: text, Profile: m.Profile}
		}
		out = append(out, m)
	}
	return out
}

// LabelOthers marks replies written by a profile other than current, so a
// model doesn't take another model's answers as its own: it would defend
// them, or "correct" ones it couldn't have made (an image it never saw, a
// web search it can't do). models maps profile names to model names; replies
// saved before profiles were recorded stay unlabeled (PRD D36).
func LabelOthers(msgs []client.Message, current string, models map[string]string) []client.Message {
	for i, m := range msgs {
		if m.Role != "assistant" || m.Profile == "" || m.Profile == current || strings.TrimSpace(m.Content) == "" {
			continue
		}
		who := "@" + m.Profile
		if model := models[m.Profile]; model != "" {
			who += " (" + model + ")"
		}
		msgs[i].Content = "[reply from " + who + ", a different model, not you]\n" + m.Content
	}
	return msgs
}

// toolNote describes a tool call without its arguments' contents: the tool
// name and, if present, the path (cut to 100 characters).
func toolNote(c client.ToolCall) string {
	var args struct {
		Path string `json:"path"`
	}
	json.Unmarshal([]byte(c.Function.Arguments), &args)
	target := ""
	if p := args.Path; p != "" {
		if len(p) > 100 {
			p = p[:100] + "..."
		}
		target = " " + p
	}
	return fmt.Sprintf("[%s%s ran on the local machine; its output is not shared with this model]", c.Function.Name, target)
}

func joinText(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "\n" + b
}
