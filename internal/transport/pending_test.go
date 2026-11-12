package transport

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestPending(t *testing.T) {
	var p Pending
	if p.Deliver("a", "x") {
		t.Error("deliver with nothing waiting")
	}
	got := make(chan string)
	go func() {
		ans, _ := p.Wait(context.Background(), "a")
		got <- ans
	}()
	for !p.Waiting("a") {
		time.Sleep(time.Millisecond)
	}
	if p.Deliver("b", "wrong chat") {
		t.Error("delivered to the wrong chat")
	}
	if !p.Deliver("a", "y") {
		t.Fatal("deliver failed")
	}
	if ans := <-got; ans != "y" {
		t.Errorf("answer = %q", ans)
	}
	if p.Waiting("a") {
		t.Error("still waiting after delivery")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := p.Wait(ctx, "a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("timeout err = %v", err)
	}
	if p.Waiting("a") {
		t.Error("waiting after timeout")
	}
}

func TestShowFooter(t *testing.T) {
	if !ShowFooter(NewCLI(strings.NewReader(""), io.Discard)) || !ShowFooter(NewWeb("127.0.0.1:0", "t")) {
		t.Error("CLI and web should show the footer")
	}
	if ShowFooter(&Telegram{}) || ShowFooter(&WhiteNoise{}) {
		t.Error("phone transports should not show the footer")
	}
}
