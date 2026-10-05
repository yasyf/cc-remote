package workspace

import (
	"cmp"
	"context"
	"log/slog"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

const (
	laneMain      = "main"
	laneMount     = "mount"
	lanePackages  = "packages"
	laneTools     = "tools"
	laneLoader    = "loader"
	laneCheckout  = "checkout"
	laneConfigure = "configure"
	laneWordnet   = "wordnet"
	laneEnroll    = "enroll"
	lanePublish   = "publish"
	laneSSH       = "ssh"
)

type span struct {
	lane  string
	phase string
	start time.Duration
	end   time.Duration
	ok    bool
}

type operation struct {
	started time.Time
	execs   atomic.Int64
	mu      sync.Mutex
	spans   []span
}

type operationKey struct{}

func (s *Session) begin(ctx context.Context) context.Context {
	return context.WithValue(ctx, operationKey{}, &operation{started: s.Now()})
}

func current(ctx context.Context) *operation {
	return ctx.Value(operationKey{}).(*operation)
}

func countExec(ctx context.Context) {
	if op, ok := ctx.Value(operationKey{}).(*operation); ok {
		op.execs.Add(1)
	}
}

func (s *Session) timed(ctx context.Context, lane, phase, machine string, run func() error) error {
	op := current(ctx)
	start := s.Now()
	err := run()
	end := s.Now()
	recorded := span{lane: lane, phase: phase, start: start.Sub(op.started), end: end.Sub(op.started), ok: err == nil}
	s.Log.Info("phase", "phase", phase, "lane", lane, "machine", machine, "start", seconds(recorded.start), "end", seconds(recorded.end), "seconds", seconds(recorded.end-recorded.start), "ok", recorded.ok)
	op.mu.Lock()
	defer op.mu.Unlock()
	op.spans = append(op.spans, recorded)
	return err
}

func (s *Session) summarize(ctx context.Context, workspace string, err error) {
	op := current(ctx)
	total := s.Now().Sub(op.started)
	op.mu.Lock()
	spans := slices.Clone(op.spans)
	op.mu.Unlock()
	slices.SortStableFunc(spans, func(a, b span) int { return cmp.Compare(a.start, b.start) })
	phases := make([]any, 0, len(spans))
	for _, recorded := range spans {
		phases = append(phases, slog.Group(recorded.phase, "lane", recorded.lane, "start", seconds(recorded.start), "end", seconds(recorded.end), "seconds", seconds(recorded.end-recorded.start), "ok", recorded.ok))
	}
	s.Log.Info("create phases", "workspace", workspace, "seconds", seconds(total), "execs", op.execs.Load(), "ok", err == nil, slog.Group("phases", phases...))
}

func seconds(d time.Duration) float64 {
	return math.Round(d.Seconds()*1000) / 1000
}
