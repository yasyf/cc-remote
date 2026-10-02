package workspace

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yasyf/cc-remote/internal/providers"
)

type tickingClock struct {
	mu    sync.Mutex
	ticks []time.Time
}

func (c *tickingClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := c.ticks[0]
	c.ticks = c.ticks[1:]
	return next
}

func at(offsets ...float64) *tickingClock {
	origin := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	clock := &tickingClock{}
	for _, offset := range offsets {
		clock.ticks = append(clock.ticks, origin.Add(time.Duration(offset*float64(time.Second))))
	}
	return clock
}

func records(t *testing.T, logged *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	scanner := bufio.NewScanner(bytes.NewReader(logged.Bytes()))
	scanner.Buffer(nil, 1<<20)
	for scanner.Scan() {
		record := map[string]any{}
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("log line %q: %v", scanner.Text(), err)
		}
		delete(record, "time")
		delete(record, "level")
		out = append(out, record)
	}
	return out
}

func TestTimedPhasesLogTheirSpanAndTheSummaryListsThem(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name  string
		clock *tickingClock
		run   func(s *Session) error
		err   error
		want  []map[string]any
	}{
		{
			name:  "one phase that succeeds",
			clock: at(0, 2, 3.5, 4),
			run: func(s *Session) error {
				return s.timed(laneCheckout, "checkout", "ws-1", func() error { return nil })
			},
			want: []map[string]any{
				{"msg": "phase", "phase": "checkout", "lane": "checkout", "machine": "ws-1", "start": 2.0, "end": 3.5, "seconds": 1.5, "ok": true},
				{"msg": "create phases", "workspace": "ws-1", "seconds": 4.0, "execs": 0.0, "ok": true, "phases": map[string]any{
					"checkout": map[string]any{"lane": "checkout", "start": 2.0, "end": 3.5, "seconds": 1.5, "ok": true},
				}},
			},
		},
		{
			name:  "one phase that fails",
			clock: at(0, 0.25, 1.2504, 2),
			run: func(s *Session) error {
				return s.timed(lanePackages, "packages", "ws-1", func() error { return boom })
			},
			err: boom,
			want: []map[string]any{
				{"msg": "phase", "phase": "packages", "lane": "packages", "machine": "ws-1", "start": 0.25, "end": 1.25, "seconds": 1.0, "ok": false},
				{"msg": "create phases", "workspace": "ws-1", "seconds": 2.0, "execs": 0.0, "ok": false, "phases": map[string]any{
					"packages": map[string]any{"lane": "packages", "start": 0.25, "end": 1.25, "seconds": 1.0, "ok": false},
				}},
			},
		},
		{
			name:  "overlapping phases on two lanes",
			clock: at(0, 1, 2, 5, 7, 9),
			run: func(s *Session) error {
				return s.timed(laneMount, "payload.stage", "ws-1", func() error {
					return s.timed(laneCheckout, "checkout", "ws-1", func() error { return nil })
				})
			},
			want: []map[string]any{
				{"msg": "phase", "phase": "checkout", "lane": "checkout", "machine": "ws-1", "start": 2.0, "end": 5.0, "seconds": 3.0, "ok": true},
				{"msg": "phase", "phase": "payload.stage", "lane": "mount", "machine": "ws-1", "start": 1.0, "end": 7.0, "seconds": 6.0, "ok": true},
				{"msg": "create phases", "workspace": "ws-1", "seconds": 9.0, "execs": 0.0, "ok": true, "phases": map[string]any{
					"payload.stage": map[string]any{"lane": "mount", "start": 1.0, "end": 7.0, "seconds": 6.0, "ok": true},
					"checkout":      map[string]any{"lane": "checkout", "start": 2.0, "end": 5.0, "seconds": 3.0, "ok": true},
				}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logged bytes.Buffer
			s := &Session{Log: slog.New(slog.NewJSONHandler(&logged, nil)), Now: tt.clock.now}
			s.begin()
			err := tt.run(s)
			if err != tt.err {
				t.Fatalf("timed = %v, want %v unchanged", err, tt.err)
			}
			s.summarize("ws-1", err)
			if got := records(t, &logged); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("records = %v\nwant %v", got, tt.want)
			}
			if len(tt.clock.ticks) != 0 {
				t.Errorf("%d clock readings left unread", len(tt.clock.ticks))
			}
		})
	}
}

func TestTheSummaryListsPhasesInStartOrder(t *testing.T) {
	var logged bytes.Buffer
	s := &Session{Log: slog.New(slog.NewJSONHandler(&logged, nil)), Now: at(0, 3, 4, 1, 2, 5).now}
	s.begin()
	for _, phase := range []string{"late", "early"} {
		if err := s.timed(laneMain, phase, "ws-1", func() error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	s.summarize("ws-1", nil)
	got := records(t, &logged)
	line := strings.Split(strings.TrimSpace(logged.String()), "\n")[len(got)-1]
	if early, late := strings.Index(line, `"early":`), strings.Index(line, `"late":`); early < 0 || late < 0 || early > late {
		t.Errorf("summary %s does not list early before late", line)
	}
}

func execsOn(calls []string, machine string) int {
	count := 0
	for _, call := range calls {
		if strings.HasPrefix(call, "exec "+machine+" ") {
			count++
		}
	}
	return count
}

func newClosureTailnetHarness(t *testing.T) *harness {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.sqfs")
	if err := os.WriteFile(path, []byte(payloadBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	return build(t, true, inventory+closureApt, fmt.Sprintf("{ payload: { path: %q, sha256: %s } }", path, sha), providers.Traits{Supervisor: providers.SupervisorSpriteEnv})
}

func TestEveryCreatePhaseLogsOnceWithItsLaneAndTheSummaryCountsTheExecs(t *testing.T) {
	plain := map[string]string{
		"machine.create": laneMain, "prerequisites": laneMain, "packages": lanePackages,
		"tools.provision": laneTools, "plugins.stage": laneTools, "tools.install": laneTools,
		"checkout": laneCheckout, "prepare": laneCheckout, "configure": laneConfigure,
		"tools.publish": lanePublish, "ssh.target": laneSSH,
	}
	payload := maps.Clone(plain)
	payload["payload.stage"], payload["payload.mount"] = laneMount, laneMount
	direct := maps.Clone(plain)
	direct["payload.url"], direct["payload.fetch"], direct["payload.mount"] = laneMain, laneMount, laneMount
	closure := maps.Clone(payload)
	closure["loader"] = laneLoader
	enrolled := maps.Clone(closure)
	enrolled["tailnet.enroll"] = laneEnroll
	tests := []struct {
		name   string
		open   func(*testing.T) *harness
		execs  int
		phases map[string]string
	}{
		{"plain", func(t *testing.T) *harness { return newHarness(t, false) }, 9, plain},
		{"plain with a tailnet", func(t *testing.T) *harness { return newHarness(t, true) }, 11, func() map[string]string {
			phases := maps.Clone(plain)
			phases["tailnet.enroll"] = laneEnroll
			return phases
		}()},
		{"imaged", newImagedHarness, 6, map[string]string{
			"machine.create": laneMain, "plugins.stage": laneTools, "tools.install": laneTools,
			"checkout": laneCheckout, "prepare": laneCheckout, "configure": laneConfigure,
			"tools.publish": lanePublish, "ssh.target": laneSSH,
		}},
		{"payload", newPayloadHarness, 11, payload},
		{"direct payload", func(t *testing.T) *harness {
			h, _ := newDirectPayloadHarness(t, "printf '%s\\n' '"+payloadURL+"'")
			return h
		}, 11, direct},
		{"closure", newClosureHarness, 12, closure},
		{"closure with a tailnet", newClosureTailnetHarness, 14, enrolled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := tt.open(t)
			var logged bytes.Buffer
			h.session.Log = slog.New(slog.NewJSONHandler(&logged, nil))
			if _, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"}); err != nil {
				t.Fatal(err)
			}
			if got := execsOn(h.fake.Calls(), "ws-1"); got != tt.execs {
				t.Errorf("the create made %d execs, want %d:\n%s", got, tt.execs, strings.Join(h.fake.Calls(), "\n"))
			}
			if got := h.machine.ran("ws-1", "enable-payload-plugins"); got != 0 {
				t.Errorf("the create ran %d separate plugin enables, want them folded into the plugins.sh staging", got)
			}
			lanes := map[string]string{}
			var summaries []map[string]any
			for _, record := range records(t, &logged) {
				switch record["msg"] {
				case "phase":
					name := record["phase"].(string)
					if _, seen := lanes[name]; seen {
						t.Errorf("phase %s logged twice", name)
					}
					lanes[name] = record["lane"].(string)
					if record["ok"] != true || record["machine"] != "ws-1" || record["start"].(float64) > record["end"].(float64) {
						t.Errorf("phase record %v", record)
					}
				case "create phases":
					summaries = append(summaries, record)
				}
			}
			if !maps.Equal(lanes, tt.phases) {
				t.Errorf("phases = %v, want %v", lanes, tt.phases)
			}
			if len(summaries) != 1 {
				t.Fatalf("%d summaries, want 1", len(summaries))
			}
			summary := summaries[0]
			listed := summary["phases"].(map[string]any)
			if got, want := slices.Sorted(maps.Keys(listed)), slices.Sorted(maps.Keys(tt.phases)); !slices.Equal(got, want) {
				t.Errorf("the summary lists %v, want %v", got, want)
			}
			for name, span := range listed {
				if span.(map[string]any)["lane"] != tt.phases[name] {
					t.Errorf("the summary puts %s on lane %v, want %s", name, span.(map[string]any)["lane"], tt.phases[name])
				}
			}
			if summary["workspace"] != "ws-1" || summary["ok"] != true || summary["execs"] != float64(tt.execs) {
				t.Errorf("summary = %v, want ws-1, ok, %d execs", summary, tt.execs)
			}
			if strings.Contains(logged.String(), token) {
				t.Error("a timing record carries the GitHub token")
			}
		})
	}
}

func TestAFailedCreateStillLogsItsSummary(t *testing.T) {
	h := newHarness(t, false)
	h.provider.unreachable.Store(true)
	var logged bytes.Buffer
	h.session.Log = slog.New(slog.NewJSONHandler(&logged, nil))
	if _, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"}); err == nil {
		t.Fatal("the create succeeded")
	}
	var summary, target map[string]any
	for _, record := range records(t, &logged) {
		switch {
		case record["msg"] == "create phases":
			summary = record
		case record["msg"] == "phase" && record["phase"] == "ssh.target":
			target = record
		}
	}
	if summary == nil || summary["ok"] != false || target == nil || target["ok"] != false {
		t.Errorf("summary %v, ssh.target %v; want both logged with ok=false", summary, target)
	}
}

func TestTheSSHTargetResolvesWhilePublishIsInFlight(t *testing.T) {
	tests := []struct {
		name    string
		publish providers.Result
		created bool
	}{
		{"publish succeeds", providers.Result{}, true},
		{"publish fails", providers.Result{Stderr: []byte("publish refused"), ExitCode: 1}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, false)
			publish := h.machine.hold(t, publishes)
			targeted := make(chan struct{})
			var once sync.Once
			h.provider.onSSH = func() { once.Do(func() { close(targeted) }) }
			created := make(chan error, 1)
			go func() {
				_, err := h.session.Create(context.Background(), "ws-1", Source{Ref: "main"})
				created <- err
			}()
			publish.awaitEntered(t)
			select {
			case <-targeted:
			case <-time.After(5 * time.Second):
				t.Fatal("the SSH target did not resolve while publish was in flight")
			}
			fragment := h.session.State.SSH("ws-1")
			if _, err := os.Stat(fragment); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the SSH fragment exists before publish finished: %v", err)
			}
			publish.release <- tt.publish
			err := <-created
			if (err == nil) != tt.created {
				t.Fatalf("Create = %v, want success %v", err, tt.created)
			}
			_, statErr := os.Stat(fragment)
			if tt.created && statErr != nil {
				t.Errorf("the created workspace has no SSH fragment: %v", statErr)
			}
			if !tt.created && !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("the failed create left its SSH fragment: %v", statErr)
			}
		})
	}
}
