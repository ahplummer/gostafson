package sessions

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ahplummer/gostafson/internal/client"
)

func user(t string) client.Message      { return client.Message{Role: "user", Content: t} }
func assistant(t string) client.Message { return client.Message{Role: "assistant", Content: t} }

// History from before conversations becomes one conversation per chat, with
// the chat's ID, so no message moves.
func TestMigrateOldHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")
	s, _ := Open(path)
	s.db.Exec(`DELETE FROM conversations`) // as an old database had none
	s.db.Exec(`INSERT INTO messages (chat_id, created_at, message) VALUES ('tg:7', '2026-11-12T10:00:00Z', '{"role":"user","content":"what oil?"}')`)
	s.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	c, err := s.Current(ctx, "tg:7")
	if err != nil || c.ID != "tg:7" || c.Title != "Earlier chat on Telegram" || c.Messages != 1 {
		t.Fatalf("current = %+v, %v", c, err)
	}
	if got, _ := s.List(ctx, "oil", 10); len(got) != 1 || got[0].ID != "tg:7" {
		t.Errorf("search after migration = %+v", got)
	}
}

func TestConversations(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	a, _ := s.Current(ctx, "tg:1")
	s.Append(ctx, a.ID, user("what engine oil for the van?"), assistant("5W-20 synthetic"))
	s.SetTitle(ctx, a.ID, `"Van oil"`)
	s.SetState(ctx, a.ID, "sonnet", "travato")
	time.Sleep(5 * time.Millisecond)

	b, _ := s.New(ctx, "tg:1") // a new one; the old one is kept
	if b.ID == a.ID {
		t.Fatal("New returned the same conversation")
	}
	s.Append(ctx, b.ID, user("tell me a joke about skeletons"), assistant("no guts"))
	if cur, _ := s.Current(ctx, "tg:1"); cur.ID != b.ID {
		t.Errorf("current = %s, want the new one", cur.ID)
	}

	list, _ := s.List(ctx, "", 10)
	if len(list) != 2 || list[0].ID != b.ID || list[1].Title != "Van oil" || list[1].Profile != "sonnet" || list[1].Project != "travato" {
		t.Fatalf("list = %+v", list)
	}
	if got, _ := s.List(ctx, "engine oil", 10); len(got) != 1 || got[0].ID != a.ID {
		t.Errorf("search = %+v", got)
	}
	if got, _ := s.List(ctx, `oil" OR *`, 10); len(got) != 1 {
		t.Errorf("odd search = %+v", got) // FTS syntax is just text
	}

	// The CLI resumes the conversation started on Telegram.
	if err := s.Switch(ctx, "cli", a.ID); err != nil {
		t.Fatal(err)
	}
	if cur, _ := s.Current(ctx, "cli"); cur.ID != a.ID || cur.Messages != 2 {
		t.Errorf("cli current = %+v", cur)
	}

	// An empty conversation isn't listed.
	s.New(ctx, "web")
	if list, _ := s.List(ctx, "", 10); len(list) != 2 {
		t.Errorf("empty conversation listed: %d", len(list))
	}

	s.Delete(ctx, a.ID)
	if _, err := s.Get(ctx, a.ID); err == nil {
		t.Error("deleted conversation still there")
	}
	if cur, _ := s.Current(ctx, "cli"); cur.ID == a.ID {
		t.Error("cli still on the deleted conversation")
	}
	if got, _ := s.List(ctx, "engine", 10); len(got) != 0 {
		t.Error("deleted conversation still found by search")
	}
}

// Messages that fell out of the recent window and aren't summarized yet.
func TestUnsummarized(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	c, _ := s.Current(ctx, "cli")
	for i := 0; i < 5; i++ {
		s.Append(ctx, c.ID, user("q"), assistant("a"))
	}
	old, err := s.Unsummarized(ctx, c.ID, 4) // keep the last 4 of 10
	if err != nil || len(old) != 6 {
		t.Fatalf("unsummarized = %d, %v", len(old), err)
	}
	s.SetSummary(ctx, c.ID, "they asked q five times", old[len(old)-1].ID)
	if more, _ := s.Unsummarized(ctx, c.ID, 4); len(more) != 0 {
		t.Errorf("after summary: %d more", len(more))
	}
	if txt := TranscriptText(old[:2]); !strings.Contains(txt, "user: q") || !strings.Contains(txt, "assistant: a") {
		t.Errorf("transcript = %q", txt)
	}
}

// Every message is in the summary or in the history sent with a turn: none
// falls between the recent window and the next summary.
func TestContextHasNoGap(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	c, _ := s.Current(ctx, "cli")
	for i := 0; i < 5; i++ { // 10 messages; window 4: 6 are past it, not yet summarized
		s.Append(ctx, c.ID, user(fmt.Sprintf("fact %d", i)), assistant("ok"))
	}
	c, _ = s.Get(ctx, c.ID)
	msgs, _ := s.Context(ctx, c, 4)
	if len(msgs) != 10 || msgs[0].Content != "fact 0" {
		t.Fatalf("before the summary: %d messages, first %q", len(msgs), msgs[0].Content)
	}
	old, _ := s.Unsummarized(ctx, c.ID, 4)
	s.SetSummary(ctx, c.ID, "facts 0 to 2", old[len(old)-1].ID)
	c, _ = s.Get(ctx, c.ID)
	msgs, _ = s.Context(ctx, c, 4)
	if len(msgs) != 4 || msgs[0].Content != "fact 3" {
		t.Errorf("after the summary: %d messages, first %q", len(msgs), msgs[0].Content)
	}
}
