package supervisor

import (
	"context"
	"fmt"
	"regexp"
	"testing"
	"time"

	supervisorv1 "github.com/andreabedini/minecraft-operator/gen/supervisor/v1"
)

func TestConsoleRingAndReplay(t *testing.T) {
	c := NewConsole(3)
	for i := 0; i < 5; i++ {
		c.Append(supervisorv1.Stream_STREAM_STDOUT, fmt.Sprintf("line %d", i))
	}
	recent := c.Recent(10)
	if len(recent) != 3 {
		t.Fatalf("Recent = %d lines, want 3", len(recent))
	}
	if recent[0].Text != "line 2" || recent[2].Text != "line 4" {
		t.Errorf("unexpected order: %+v", recent)
	}
	if recent[0].Seq != 2 {
		t.Errorf("seq = %d, want 2", recent[0].Seq)
	}

	ch, cancel := c.Subscribe(2)
	defer cancel()
	got := []string{(<-ch).Text, (<-ch).Text}
	if got[0] != "line 3" || got[1] != "line 4" {
		t.Errorf("replay = %v", got)
	}
	c.Append(supervisorv1.Stream_STREAM_STDERR, "live")
	select {
	case l := <-ch:
		if l.Text != "live" || l.Stream != supervisorv1.Stream_STREAM_STDERR {
			t.Errorf("live line = %+v", l)
		}
	case <-time.After(time.Second):
		t.Fatal("no live line")
	}
	cancel()
	if _, ok := <-ch; ok {
		t.Error("channel not closed after cancel")
	}
}

func TestConsoleWaitFor(t *testing.T) {
	c := NewConsole(10)
	c.Append(supervisorv1.Stream_STREAM_STDOUT, "Saved the game") // before WaitFor: ignored
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan Line, 1)
	go func() {
		l, err := c.WaitFor(ctx, regexp.MustCompile(`Saved the game`))
		if err != nil {
			t.Error(err)
		}
		done <- l
	}()
	time.Sleep(50 * time.Millisecond)
	c.Append(supervisorv1.Stream_STREAM_STDOUT, "noise")
	c.Append(supervisorv1.Stream_STREAM_STDOUT, "[Server thread/INFO]: Saved the game")
	select {
	case l := <-done:
		if l.Seq != 2 {
			t.Errorf("matched seq %d, want 2", l.Seq)
		}
	case <-ctx.Done():
		t.Fatal("WaitFor did not return")
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	if _, err := c.WaitFor(ctx2, regexp.MustCompile(`never`)); err == nil {
		t.Error("expected timeout error")
	}
}
