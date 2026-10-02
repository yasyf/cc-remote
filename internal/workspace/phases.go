package workspace

import (
	"cmp"
	"log/slog"
	"math"
	"slices"
	"sync"
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

type timeline struct {
	started time.Time
	mu      sync.Mutex
	spans   []span
}

func (s *Session) begin() {
	s.timeline = &timeline{started: s.Now()}
}

func (s *Session) timed(lane, phase, machine string, run func() error) error {
	start := s.Now()
	err := run()
	end := s.Now()
	line := s.timeline
	recorded := span{lane: lane, phase: phase, start: start.Sub(line.started), end: end.Sub(line.started), ok: err == nil}
	s.Log.Info("phase", "phase", phase, "lane", lane, "machine", machine, "start", seconds(recorded.start), "end", seconds(recorded.end), "seconds", seconds(recorded.end-recorded.start), "ok", recorded.ok)
	line.mu.Lock()
	defer line.mu.Unlock()
	line.spans = append(line.spans, recorded)
	return err
}

func (s *Session) summarize(workspace string, err error) {
	line := s.timeline
	total := s.Now().Sub(line.started)
	line.mu.Lock()
	spans := slices.Clone(line.spans)
	line.mu.Unlock()
	slices.SortStableFunc(spans, func(a, b span) int { return cmp.Compare(a.start, b.start) })
	phases := make([]any, 0, len(spans))
	for _, recorded := range spans {
		phases = append(phases, slog.Group(recorded.phase, "lane", recorded.lane, "start", seconds(recorded.start), "end", seconds(recorded.end), "seconds", seconds(recorded.end-recorded.start), "ok", recorded.ok))
	}
	s.Log.Info("create phases", "workspace", workspace, "seconds", seconds(total), "execs", s.execs.Load(), "ok", err == nil, slog.Group("phases", phases...))
}

func seconds(d time.Duration) float64 {
	return math.Round(d.Seconds()*1000) / 1000
}
