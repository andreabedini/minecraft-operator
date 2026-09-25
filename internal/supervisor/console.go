package supervisor

import (
	"context"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	supervisorv1 "github.com/andreabedini/minecraft-operator/gen/supervisor/v1"
)

// Line is one console line, from the process or from the supervisor itself.
type Line struct {
	Seq    uint64
	Time   time.Time
	Stream supervisorv1.Stream
	Text   string
}

// Console keeps a ring buffer of recent lines and fans new lines out to
// subscribers. Slow subscribers lose lines rather than block the process.
type Console struct {
	mu       sync.Mutex
	capacity int
	buf      []Line
	start    int // index of the oldest line
	next     uint64
	subs     map[*subscriber]struct{}
}

type subscriber struct {
	ch      chan Line
	dropped atomic.Uint64
	closed  bool
}

// NewConsole returns a console keeping the last capacity lines.
func NewConsole(capacity int) *Console {
	if capacity < 1 {
		capacity = 1
	}
	return &Console{
		capacity: capacity,
		buf:      make([]Line, 0, capacity),
		subs:     make(map[*subscriber]struct{}),
	}
}

// Append records a line and delivers it to subscribers.
func (c *Console) Append(stream supervisorv1.Stream, text string) Line {
	c.mu.Lock()
	defer c.mu.Unlock()
	line := Line{Seq: c.next, Time: time.Now(), Stream: stream, Text: text}
	c.next++
	if len(c.buf) < c.capacity {
		c.buf = append(c.buf, line)
	} else {
		c.buf[c.start] = line
		c.start = (c.start + 1) % c.capacity
	}
	for s := range c.subs {
		select {
		case s.ch <- line:
		default:
			s.dropped.Add(1)
		}
	}
	return line
}

// Subscribe returns a channel that first yields up to replay buffered lines,
// then live lines. cancel must be called to release the subscription; the
// channel is closed afterwards.
func (c *Console) Subscribe(replay int) (<-chan Line, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if replay < 0 {
		replay = 0
	}
	if replay > len(c.buf) {
		replay = len(c.buf)
	}
	s := &subscriber{ch: make(chan Line, replay+1024)}
	n := len(c.buf)
	for i := n - replay; i < n; i++ {
		s.ch <- c.buf[(c.start+i)%len(c.buf)]
	}
	c.subs[s] = struct{}{}
	cancel := func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if s.closed {
			return
		}
		s.closed = true
		delete(c.subs, s)
		close(s.ch)
	}
	return s.ch, cancel
}

// Recent returns up to n most recent lines, oldest first.
func (c *Console) Recent(n int) []Line {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n > len(c.buf) {
		n = len(c.buf)
	}
	out := make([]Line, 0, n)
	total := len(c.buf)
	for i := total - n; i < total; i++ {
		out = append(out, c.buf[(c.start+i)%len(c.buf)])
	}
	return out
}

// WaitFor blocks until a line matching re arrives or ctx is done. Lines
// appended before the call are not considered.
func (c *Console) WaitFor(ctx context.Context, re *regexp.Regexp) (Line, error) {
	ch, cancel := c.Subscribe(0)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return Line{}, ctx.Err()
		case line, ok := <-ch:
			if !ok {
				return Line{}, context.Canceled
			}
			if re.MatchString(line.Text) {
				return line, nil
			}
		}
	}
}
