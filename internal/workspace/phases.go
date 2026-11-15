package workspace

import (
	"cmp"
	"context"
	"errors"
	"fmt"
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
	lanePrefetch  = "prefetch"
	lanePackages  = "packages"
	laneTools     = "tools"
	laneLoader    = "loader"
	laneCheckout  = "checkout"
	laneConfigure = "configure"
	laneEnroll    = "enroll"
	lanePublish   = "publish"
	laneSSH       = "ssh"
)

var phaseWithin = map[string]time.Duration{
	"machine.create":         10 * time.Minute,
	"image.adopt":            2 * time.Minute,
	"payload.boot":           payloadMountWithin,
	"memory.limit":           2 * time.Minute,
	"local.disk":             5 * time.Minute,
	"prerequisites":          15 * time.Minute,
	"packages":               15 * time.Minute,
	"loader":                 5 * time.Minute,
	"tools.provision":        10 * time.Minute,
	"plugins.stage":          5 * time.Minute,
	"tools.install":          10 * time.Minute,
	"payload.url":            5 * time.Minute,
	"payload.fetch":          20 * time.Minute,
	"payload.stage":          20 * time.Minute,
	"packages.url":           5 * time.Minute,
	"packages.fetch":         20 * time.Minute,
	"packages.stage":         20 * time.Minute,
	"bootstrap-helper.url":   5 * time.Minute,
	"bootstrap-helper.fetch": 5 * time.Minute,
	"bootstrap-helper.stage": 5 * time.Minute,
	"payload.mount":          5 * time.Minute,
	"payload.prefetch":       10 * time.Minute,
	"tools.publish":          5 * time.Minute,
	"checkout":               15 * time.Minute,
	"prepare":                30 * time.Minute,
	"configure":              15 * time.Minute,
	"tailnet.enroll":         10 * time.Minute,
	"ssh.target":             5 * time.Minute,
}

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

type PhaseError struct {
	Phase string
	Err   error
}

func (e *PhaseError) Error() string { return e.Err.Error() }

func (e *PhaseError) Unwrap() error { return e.Err }

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

func (s *Session) timed(ctx context.Context, lane, phase, machine string, run func(context.Context) error) error {
	op := current(ctx)
	within, ok := phaseWithin[phase]
	if !ok {
		panic("phase " + phase + " has no deadline")
	}
	bounded, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	start := s.Now()
	err := run(bounded)
	if err != nil && ctx.Err() == nil && errors.Is(bounded.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("%s on %s did not finish within %s: %w", phase, machine, within, err)
	}
	end := s.Now()
	recorded := span{lane: lane, phase: phase, start: start.Sub(op.started), end: end.Sub(op.started), ok: err == nil}
	s.Log.Info("phase", "phase", phase, "lane", lane, "machine", machine, "start", seconds(recorded.start), "end", seconds(recorded.end), "seconds", seconds(recorded.end-recorded.start), "ok", recorded.ok)
	op.mu.Lock()
	defer op.mu.Unlock()
	op.spans = append(op.spans, recorded)
	if err != nil {
		return &PhaseError{Phase: phase, Err: err}
	}
	return nil
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
