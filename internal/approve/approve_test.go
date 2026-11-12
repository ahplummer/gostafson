package approve

import (
	"context"
	"io"
	"testing"
	"time"
)

type fakeAsker struct {
	answer string
	err    error
	wait   bool // block until ctx is done
	asked  string
}

func (f *fakeAsker) Ask(ctx context.Context, _, q string) (string, error) {
	f.asked = q
	if f.wait {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return f.answer, f.err
}

func TestApprove(t *testing.T) {
	ctx := context.Background()
	for answer, want := range map[string]Result{
		"y": Approved, "YES": Approved, " yes ": Approved,
		"": Denied, "n": Denied, "no": Denied, "sure": Denied, "yes please": Denied,
	} {
		f := &fakeAsker{answer: answer}
		if got := New(f, time.Second).Approve(ctx, "c", "overwrite a.txt?"); got != want {
			t.Errorf("answer %q = %s, want %s", answer, got, want)
		}
		if f.asked != "overwrite a.txt? [y/N]" {
			t.Errorf("asked %q", f.asked)
		}
	}
}

func TestTimeoutMeansNo(t *testing.T) {
	start := time.Now()
	got := New(&fakeAsker{wait: true}, 20*time.Millisecond).Approve(context.Background(), "c", "q")
	if got != Timeout {
		t.Errorf("got %s, want timeout", got)
	}
	if time.Since(start) > time.Second {
		t.Error("did not time out promptly")
	}
}

func TestErrorsAndNil(t *testing.T) {
	if got := New(&fakeAsker{err: io.EOF}, time.Second).Approve(context.Background(), "c", "q"); got != Denied {
		t.Errorf("EOF = %s, want denied", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := New(&fakeAsker{wait: true}, time.Second).Approve(ctx, "c", "q"); got != Denied {
		t.Errorf("cancelled turn = %s, want denied", got)
	}
	var g *Gate
	if got := g.Approve(context.Background(), "c", "q"); got != Unavailable {
		t.Errorf("nil gate = %s", got)
	}
	if got := New(nil, time.Second).Approve(context.Background(), "c", "q"); got != Unavailable {
		t.Errorf("nil asker = %s", got)
	}
}
