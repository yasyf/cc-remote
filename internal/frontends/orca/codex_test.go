package orca_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-remote/internal/frontends/orca"
)

const (
	sendTrust   = "terminal send --terminal term-1 --text t" + scope
	sendEscape  = "terminal send --terminal term-1 --text " + orca.KeyEscape + scope
	idleWait    = `{"wait":{"handle":"term-1","condition":"tui-idle","satisfied":true,"status":"running","exitCode":null}}`
	blockedWait = `{"wait":{"handle":"term-1","condition":"tui-idle","satisfied":false,"status":"running","exitCode":null,"blockedReason":"agent-hooks-review-prompt"}}`
	probeHome   = "/home/agent"
	probeRoot   = probeHome + "/.claude/plugins/cache/captain-hook/captain-hook/12.79.15"
	probeDigest = "1111111111111111111111111111111111111111111111111111111111111111"
	leaked      = "sk-synthetic-credential-0000"
)

var (
	captainPins = []orca.Pin{{ID: "captain-hook@captain-hook", Version: "12.79.15"}}
	exactProbe  = `{"schema":1,"outcome":"exact","reason":"","event":"","record":-1,"home":"` + probeHome + `","root":"` + probeRoot + `","hooksSha256":"` + probeDigest + `","definitions":13,"events":{"SessionStart":2,"UserPromptSubmit":2,"PreToolUse":2,"PermissionRequest":1,"PostToolUse":2,"SubagentStart":1,"SubagentStop":1,"Stop":2}}`
	codexBanner = []string{"  >_ OpenAI Codex (v0.159.2)", "     ~/app", "  permissions: YOLO mode"}
	refusalPoll = orca.Poll{Interval: time.Millisecond, Timeout: time.Second}
)

type browserRow struct {
	event       string
	installed   int
	active      int
	review      int
	description string
}

var acceptedRows = []browserRow{
	{"PreToolUse", 2, 1, 1, "Before a tool executes"},
	{"PermissionRequest", 1, 1, 0, "When permission is requested"},
	{"PostToolUse", 2, 1, 1, "After a tool executes"},
	{"PreCompact", 0, 0, 0, "Before context compaction"},
	{"PostCompact", 0, 0, 0, "After context compaction"},
	{"SessionStart", 2, 1, 1, "When a new session starts"},
	{"SessionEnd", 0, 0, 0, "Right before a session ends"},
	{"UserPromptSubmit", 2, 1, 1, "When the user submits a prompt"},
	{"SubagentStart", 1, 1, 0, "When a subagent is created"},
	{"SubagentStop", 1, 1, 0, "Right before a subagent ends its turn"},
	{"Stop", 2, 1, 1, "Right before Codex ends its turn"},
	{"Interrupt", 0, 0, 0, "Right before an interrupted turn is aborted"},
}

func frameOf(t *testing.T, source string, truncated, limited bool, lines []string) string {
	t.Helper()
	tail, err := json.Marshal(lines)
	if err != nil {
		t.Fatal(err)
	}
	return ok(fmt.Sprintf(`{"terminal":{"handle":"term-1","status":"running","source":%q,"truncated":%t,"limited":%t,"tail":%s}}`, source, truncated, limited, tail))
}

func historyLost(t *testing.T, lines []string) string {
	t.Helper()
	return frameOf(t, "screen", true, false, lines)
}

func promptScreen(pending int, selected string) []string {
	lines := slices.Clone(codexBanner)
	lines = append(lines, "  Hooks need review", fmt.Sprintf("  %d hooks are new or changed.", pending), "  Hooks can run outside the sandbox after you trust them.")
	for _, option := range []string{"1. Review hooks", "2. Trust all and continue", "3. Continue without trusting (hooks won't run)"} {
		marker := "  "
		if option == selected {
			marker = "› "
		}
		lines = append(lines, marker+option)
	}
	return append(lines, "  enter confirm · esc skip")
}

func browserScreen(rows []browserRow, top, selected, window, pending int, below bool) []string {
	lines := slices.Clone(codexBanner)
	lines = append(lines, "  Hooks", "  Lifecycle hooks from config and enabled plugins.", fmt.Sprintf("  ⚠ %d hooks need review before they can run.", pending), "  Event                 Installed   Active      Review      Description")
	if top > 0 {
		lines = append(lines, "↑")
	}
	end := min(top+window, len(rows))
	for i := top; i < end; i++ {
		marker := " "
		if i == selected {
			marker = "›"
		}
		row := rows[i]
		lines = append(lines, fmt.Sprintf("%s %-21s %-11d %-11d %-11d %s", marker, row.event, row.installed, row.active, row.review, row.description))
	}
	if below {
		lines = append(lines, "↓")
	}
	return append(lines, "  t trust all · enter review · esc close")
}

func trustedScreen(rows []browserRow, window int) []string {
	lines := slices.Clone(codexBanner)
	lines = append(lines, "  Hooks", "  Lifecycle hooks from config and enabled plugins.", "  Event                 Installed   Active      Description")
	for i, row := range rows[:window] {
		marker := " "
		if i == 0 {
			marker = "›"
		}
		lines = append(lines, fmt.Sprintf("%s %-21s %-11d %-11d %s", marker, row.event, row.installed, row.installed, row.description))
	}
	return append(lines, "↓", "  enter details · esc close")
}

type walkOptions struct {
	rows       []browserRow
	window     int
	pending    int
	alwaysMore bool
}

func walk(t *testing.T, o walkOptions) ([]string, int) {
	t.Helper()
	top, selected := 0, 0
	more := func() bool { return o.alwaysMore || top+o.window < len(o.rows) }
	view := func() string {
		return historyLost(t, browserScreen(o.rows, top, selected, o.window, o.pending, more()))
	}
	screens := []string{view()}
	downs := 0
	for more() && selected+1 < len(o.rows) {
		selected++
		if selected >= top+o.window {
			top = selected - o.window + 1
		}
		screens = append(screens, view())
		downs++
	}
	for range downs {
		selected--
		if selected < top {
			top = selected
		}
		screens = append(screens, view())
	}
	return screens, downs
}

type probeRecorder struct {
	t     *testing.T
	fake  *fakeOrca
	out   string
	calls int
}

func (p *probeRecorder) review(captain []orca.Pin) orca.HookReview {
	return orca.HookReview{Captain: captain, Exec: func(_ context.Context, argv []string) ([]byte, error) {
		p.calls++
		if p.fake.called(sendEnter) != 0 || p.fake.called(sendTrust) != 0 {
			p.t.Error("the probe ran after review input")
		}
		if len(argv) != 5 || argv[0] != "python3" || argv[1] != "-c" || argv[2] != orca.HookProbeScript || argv[3] != captain[0].ID || argv[4] != captain[0].Version {
			p.t.Errorf("probe argv = %q", argv)
		}
		return []byte(p.out), nil
	}}
}

func codexFake(t *testing.T) *fakeOrca {
	return newFakeOrca(t).on(sendEnter, ok(accepted)).on(sendDown, ok(accepted)).on(sendUp, ok(accepted)).
		on(sendTrust, ok(accepted)).on(sendEscape, ok(accepted)).on(waitIdle, ok(idleWait))
}

func position(f *fakeOrca, command string, nth int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, call := range f.calls {
		if call == command {
			if nth == 0 {
				return i
			}
			nth--
		}
	}
	return -1
}

func lastPosition(f *fakeOrca, command string) int {
	return position(f, command, f.called(command)-1)
}

func runCodex(t *testing.T, fake *fakeOrca, review orca.HookReview, p orca.Poll) ([]string, error) {
	t.Helper()
	return orca.NewClient(fake).On(env, runtimeID).Bootstrap(context.Background(), "term-1", orca.CodexStartup(review), false, p)
}

func assertNoTrust(t *testing.T, fake *fakeOrca) {
	t.Helper()
	if fake.called(sendTrust) != 0 || fake.called(sendEscape) != 0 {
		t.Errorf("the refused review sent trust %d times and Escape %d times", fake.called(sendTrust), fake.called(sendEscape))
	}
}

func assertSent(t *testing.T, fake *fakeOrca, sent map[string]int) {
	t.Helper()
	for command, want := range sent {
		if got := fake.called(command); got != want {
			t.Errorf("%q called %d times, want %d", command, got, want)
		}
	}
}

func acceptedReads(t *testing.T) ([]string, int) {
	t.Helper()
	screens, downs := walk(t, walkOptions{rows: acceptedRows, window: 8, pending: 5})
	return slices.Concat([]string{historyLost(t, promptScreen(5, ""))}, screens, []string{historyLost(t, trustedScreen(acceptedRows, 8))}), downs
}

func otherTerminal(reply string) string {
	return strings.Replace(reply, `"handle":"term-1"`, `"handle":"term-2"`, 1)
}

type stall struct {
	t        *testing.T
	budget   time.Duration
	deadline time.Time
}

func (s *stall) until(ctx context.Context) error {
	s.t.Helper()
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > s.budget {
		s.t.Errorf("the stalled call may run until %v (deadline set %t), not within %s", deadline, ok, s.budget)
		return errors.New("the stalled call has no review deadline")
	}
	s.deadline = deadline
	<-ctx.Done()
	return ctx.Err()
}

type stallingOrca struct {
	fake    *fakeOrca
	stall   *stall
	command string
}

func (s stallingOrca) Run(ctx context.Context, args ...string) ([]byte, error) {
	out, err := s.fake.Run(ctx, args...)
	if strings.Join(args, " ") != s.command {
		return out, err
	}
	return nil, s.stall.until(ctx)
}

func TestCodexReviewWalksEveryRowResetsAndTrustsOnce(t *testing.T) {
	screens, downs := walk(t, walkOptions{rows: acceptedRows, window: 8, pending: 5})
	fake := codexFake(t).on(readScreen, historyLost(t, promptScreen(5, "")))
	for _, screen := range screens {
		fake.on(readScreen, screen)
	}
	fake.on(readScreen, historyLost(t, trustedScreen(acceptedRows, 8)))
	probe := &probeRecorder{t: t, fake: fake, out: exactProbe}
	steps, err := runCodex(t, fake, probe.review(captainPins), bootstrapPoll)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(steps, []string{"hooks"}) || probe.calls != 1 || downs != 11 {
		t.Fatalf("steps %v, probes %d, downs %d", steps, probe.calls, downs)
	}
	assertSent(t, fake, map[string]int{sendEnter: 1, sendDown: downs, sendUp: downs, sendTrust: 1, sendEscape: 1, waitIdle: 1})
	enter, firstDown, lastDown, firstUp, lastUp := position(fake, sendEnter, 0), position(fake, sendDown, 0), lastPosition(fake, sendDown), position(fake, sendUp, 0), lastPosition(fake, sendUp)
	trust, escape, idle := position(fake, sendTrust, 0), position(fake, sendEscape, 0), position(fake, waitIdle, 0)
	if !(enter < firstDown && lastDown < firstUp && lastUp < trust && trust < escape && escape < idle) {
		t.Errorf("order enter %d, downs %d-%d, ups %d-%d, trust %d, escape %d, idle %d", enter, firstDown, lastDown, firstUp, lastUp, trust, escape, idle)
	}
	for i := range downs {
		if position(fake, readScreen, 2+i) > position(fake, sendDown, i+1) && i+1 < downs {
			t.Errorf("down %d was sent before the previous move was observed", i+1)
		}
	}
}

func TestCodexReviewDeduplicatesOverlappingViews(t *testing.T) {
	screens, downs := walk(t, walkOptions{rows: acceptedRows, window: 4, pending: 5})
	fake := codexFake(t).on(readScreen, historyLost(t, promptScreen(5, "1. Review hooks")))
	for _, screen := range screens {
		fake.on(readScreen, screen)
	}
	fake.on(readScreen, historyLost(t, trustedScreen(acceptedRows, 4)))
	probe := &probeRecorder{t: t, fake: fake, out: exactProbe}
	if _, err := runCodex(t, fake, probe.review(captainPins), bootstrapPoll); err != nil {
		t.Fatal(err)
	}
	if fake.called(sendDown) != downs || fake.called(sendUp) != downs || fake.called(sendTrust) != 1 {
		t.Errorf("downs %d, ups %d, trust %d with %d overlapping views", fake.called(sendDown), fake.called(sendUp), fake.called(sendTrust), downs)
	}
}

func TestCodexAlreadyTrustedSessionSendsNoTrustInput(t *testing.T) {
	fake := codexFake(t).on(readScreen, historyLost(t, append(slices.Clone(codexBanner), "  › Ask Codex anything")))
	probe := &probeRecorder{t: t, fake: fake, out: exactProbe}
	steps, err := runCodex(t, fake, probe.review(captainPins), bootstrapPoll)
	if err != nil || len(steps) != 0 || probe.calls != 0 || fake.called(waitIdle) != 1 {
		t.Fatalf("steps %v, err %v, probes %d", steps, err, probe.calls)
	}
	for _, command := range []string{sendEnter, sendDown, sendUp, sendTrust, sendEscape} {
		if fake.called(command) != 0 {
			t.Errorf("an already trusted session sent %q", command)
		}
	}
}

func TestCodexReviewStartsAfterTheIdleWaitReportsThePrompt(t *testing.T) {
	screens, _ := walk(t, walkOptions{rows: acceptedRows, window: 8, pending: 5})
	fake := newFakeOrca(t).on(sendEnter, ok(accepted)).on(sendDown, ok(accepted)).on(sendUp, ok(accepted)).
		on(sendTrust, ok(accepted)).on(sendEscape, ok(accepted)).on(waitIdle, ok(blockedWait)).on(waitIdle, ok(idleWait)).
		on(readScreen, historyLost(t, codexBanner)).on(readScreen, historyLost(t, promptScreen(5, "")))
	for _, screen := range screens {
		fake.on(readScreen, screen)
	}
	fake.on(readScreen, historyLost(t, trustedScreen(acceptedRows, 8)))
	probe := &probeRecorder{t: t, fake: fake, out: exactProbe}
	if _, err := runCodex(t, fake, probe.review(captainPins), bootstrapPoll); err != nil {
		t.Fatal(err)
	}
	if fake.called(waitIdle) != 2 || fake.called(sendTrust) != 1 || position(fake, waitIdle, 0) > position(fake, sendEnter, 0) {
		t.Errorf("waits %d, trust %d", fake.called(waitIdle), fake.called(sendTrust))
	}
}

func TestCodexReviewRefusesBeforeAnyInput(t *testing.T) {
	refused := `{"schema":1,"outcome":"refused","reason":"mismatch","event":"Stop","record":1}`
	tests := []struct {
		name    string
		screen  string
		captain []orca.Pin
		probe   string
		want    string
		probes  int
	}{
		{"limited prompt", frameOf(t, "screen", false, true, promptScreen(5, "")), captainPins, exactProbe, "limited screen", 0},
		{"limited prompt with lost history", frameOf(t, "screen", true, true, promptScreen(5, "")), captainPins, exactProbe, "limited screen", 0},
		{"unknown source", frameOf(t, "stream", false, false, promptScreen(5, "")), captainPins, exactProbe, "not a rendered screen", 0},
		{"another runtime", strings.Replace(historyLost(t, promptScreen(5, "")), `"runtimeId":"`+runtimeID+`"`, `"runtimeId":"rt-2"`, 1), captainPins, exactProbe, `answered from runtime "rt-2"`, 0},
		{"old pilot count", historyLost(t, promptScreen(10, "")), captainPins, exactProbe, "counts 10 new hooks", 0},
		{"another selected action", historyLost(t, promptScreen(5, "2. Trust all and continue")), captainPins, exactProbe, "other than Review hooks", 0},
		{"no Captain pin", historyLost(t, promptScreen(5, "")), nil, exactProbe, "selects 0 Captain Hook plugins", 0},
		{"two Captain pins", historyLost(t, promptScreen(5, "")), append(slices.Clone(captainPins), orca.Pin{ID: "captain-hook@other", Version: "1.0.0"}), exactProbe, "selects 2 Captain Hook plugins", 0},
		{"probe refusal", historyLost(t, promptScreen(5, "")), captainPins, refused, "mismatch at Stop record 1", 1},
		{"probe root for another home", historyLost(t, promptScreen(5, "")), captainPins, strings.Replace(exactProbe, `"root":"`+probeRoot, `"root":"/home/sprite/.claude/plugins/cache/captain-hook/captain-hook/12.79.15`, 1), "another Captain Hook root", 1},
		{"probe counts", historyLost(t, promptScreen(5, "")), captainPins, strings.Replace(exactProbe, `"Stop":2`, `"Stop":3`, 1), "another set of definitions", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := codexFake(t).on(readScreen, tt.screen)
			probe := &probeRecorder{t: t, fake: fake, out: tt.probe}
			var review orca.HookReview
			if tt.captain == nil {
				review = orca.HookReview{Exec: func(context.Context, []string) ([]byte, error) {
					probe.calls++
					return []byte(tt.probe), nil
				}}
			} else {
				review = probe.review(tt.captain)
			}
			_, err := runCodex(t, fake, review, refusalPoll)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Bootstrap = %v, want %q", err, tt.want)
			}
			if probe.calls != tt.probes || fake.called(sendEnter) != 0 {
				t.Errorf("probes %d (want %d), Enter %d", probe.calls, tt.probes, fake.called(sendEnter))
			}
			assertNoTrust(t, fake)
		})
	}
}

func TestCodexReviewRefusesIncompleteOrChangedBrowsers(t *testing.T) {
	without := func(event string) []browserRow {
		return slices.DeleteFunc(slices.Clone(acceptedRows), func(r browserRow) bool { return r.event == event })
	}
	extra := slices.Clone(acceptedRows)
	extra[3] = browserRow{"PreCompact", 1, 0, 1, "Before context compaction"}
	unknown := append(slices.Clone(acceptedRows[:11]), browserRow{"Notification", 0, 0, 0, "Synthetic"})
	guessed := slices.Concat(acceptedRows[:8], []browserRow{acceptedRows[10], acceptedRows[8], acceptedRows[9], acceptedRows[11]})
	pilot := slices.Clone(acceptedRows)
	for i := range pilot {
		if pilot[i].installed == 2 {
			pilot[i] = browserRow{pilot[i].event, 3, 1, 2, pilot[i].description}
		}
	}
	tests := []struct {
		name    string
		options walkOptions
		edit    func([]string) []string
		want    string
	}{
		{"missing row", walkOptions{rows: without("SubagentStop"), window: 8, pending: 5}, nil, "lists no SubagentStop row"},
		{"missing PreCompact row", walkOptions{rows: without("PreCompact"), window: 8, pending: 5}, nil, "lists no PreCompact row"},
		{"missing PostCompact row", walkOptions{rows: without("PostCompact"), window: 8, pending: 5}, nil, "lists no PostCompact row"},
		{"missing SessionEnd row", walkOptions{rows: without("SessionEnd"), window: 8, pending: 5}, nil, "lists no SessionEnd row"},
		{"missing Interrupt row", walkOptions{rows: without("Interrupt"), window: 8, pending: 5}, nil, "lists no Interrupt row"},
		{"guessed row order", walkOptions{rows: guessed, window: 8, pending: 5}, nil, "out of the supported order"},
		{"extra nonzero row", walkOptions{rows: extra, window: 8, pending: 5}, nil, "PreCompact with 1 installed"},
		{"unknown row", walkOptions{rows: unknown, window: 8, pending: 5}, nil, "unsupported event row"},
		{"old pilot rows", walkOptions{rows: pilot, window: 8, pending: 5}, nil, "PreToolUse with 3 installed"},
		{"browser review total", walkOptions{rows: acceptedRows, window: 8, pending: 10}, nil, "counts 10 hooks to review, not 5"},
		{"pagination never ends", walkOptions{rows: acceptedRows, window: 8, pending: 5, alwaysMore: true}, nil, "more rows than the supported events"},
		{"counts change between views", walkOptions{rows: acceptedRows, window: 8, pending: 5}, func(s []string) []string {
			changed := slices.Clone(acceptedRows)
			changed[2] = browserRow{"PostToolUse", 2, 2, 0, "After a tool executes"}
			s[9] = historyLost(t, browserScreen(changed, 2, 9, 8, 5, true))
			return s
		}, "changed the PostToolUse counts"},
		{"limited view", walkOptions{rows: acceptedRows, window: 8, pending: 5}, func(s []string) []string {
			s[3] = frameOf(t, "screen", false, true, browserScreen(acceptedRows, 0, 3, 8, 5, true))
			return s
		}, "limited screen"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			screens, _ := walk(t, tt.options)
			if tt.edit != nil {
				screens = tt.edit(screens)
			}
			fake := codexFake(t).on(readScreen, historyLost(t, promptScreen(5, "")))
			for _, screen := range screens {
				fake.on(readScreen, screen)
			}
			probe := &probeRecorder{t: t, fake: fake, out: exactProbe}
			_, err := runCodex(t, fake, probe.review(captainPins), refusalPoll)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Bootstrap = %v, want %q", err, tt.want)
			}
			if fake.called(sendUp) != 0 {
				t.Errorf("the refused enumeration reset with %d Up inputs", fake.called(sendUp))
			}
			assertNoTrust(t, fake)
		})
	}
}

func TestCodexReviewStopsOnStuckOrUnverifiedMovement(t *testing.T) {
	screens, downs := walk(t, walkOptions{rows: acceptedRows, window: 8, pending: 5})
	tests := []struct {
		name  string
		edit  func(*fakeOrca, []string) []string
		want  string
		downs int
		ups   int
	}{
		{"stuck down", func(_ *fakeOrca, s []string) []string { return []string{s[0], s[0]} }, "selection never moved from PreToolUse", 1, 0},
		{"skipped row", func(_ *fakeOrca, s []string) []string { return append([]string{s[0]}, s[2:]...) }, "moved from PreToolUse to PostToolUse, not one row", 1, 0},
		{"stuck reset", func(_ *fakeOrca, s []string) []string { return append(slices.Clone(s[:downs+1]), s[downs]) }, "selection never moved from Interrupt", downs, 1},
		{"reset jumps", func(_ *fakeOrca, s []string) []string {
			return append(slices.Clone(s[:downs+1]), historyLost(t, browserScreen(acceptedRows, 3, 8, 8, 5, false)))
		}, "moved from Interrupt to SubagentStart, not one row", downs, 1},
		{"ambiguous reset", func(f *fakeOrca, s []string) []string {
			f.replies[sendUp] = nil
			f.fail(sendUp, "", errors.New("connection reset"))
			return s
		}, "connection reset", downs, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := codexFake(t).on(readScreen, historyLost(t, promptScreen(5, "")))
			for _, screen := range tt.edit(fake, slices.Clone(screens)) {
				fake.on(readScreen, screen)
			}
			probe := &probeRecorder{t: t, fake: fake, out: exactProbe}
			_, err := runCodex(t, fake, probe.review(captainPins), refusalPoll)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Bootstrap = %v, want %q", err, tt.want)
			}
			if fake.called(sendDown) != tt.downs || fake.called(sendUp) != tt.ups {
				t.Errorf("downs %d (want %d), ups %d (want %d)", fake.called(sendDown), tt.downs, fake.called(sendUp), tt.ups)
			}
			assertNoTrust(t, fake)
		})
	}
}

func TestCodexTrustIsSentOnceAndNeverRetried(t *testing.T) {
	screens, _ := walk(t, walkOptions{rows: acceptedRows, window: 8, pending: 5})
	tests := []struct {
		name string
		edit func(*fakeOrca)
		next string
		want string
	}{
		{"ambiguous trust", func(f *fakeOrca) {
			f.replies[sendTrust] = nil
			f.fail(sendTrust, "", errors.New("connection reset"))
		}, screens[len(screens)-1], "connection reset"},
		{"trust never lands", func(*fakeOrca) {}, screens[len(screens)-1], "did not produce the trusted hooks browser"},
		{"trusted view moved", func(*fakeOrca) {}, historyLost(t, slices.Replace(trustedScreen(acceptedRows, 8), 6, 8, strings.Replace(trustedScreen(acceptedRows, 8)[6], "›", " ", 1), strings.Replace(trustedScreen(acceptedRows, 8)[7], " ", "›", 1))), "left the first row"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := codexFake(t).on(readScreen, historyLost(t, promptScreen(5, "")))
			tt.edit(fake)
			for _, screen := range screens {
				fake.on(readScreen, screen)
			}
			fake.on(readScreen, tt.next)
			probe := &probeRecorder{t: t, fake: fake, out: exactProbe}
			_, err := runCodex(t, fake, probe.review(captainPins), refusalPoll)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Bootstrap = %v, want %q", err, tt.want)
			}
			if fake.called(sendTrust) != 1 || fake.called(sendEscape) != 0 || fake.called(waitIdle) != 0 {
				t.Errorf("trust %d, Escape %d, wait %d", fake.called(sendTrust), fake.called(sendEscape), fake.called(waitIdle))
			}
		})
	}
}

func TestCodexReviewRefusesRepliesForAnotherTerminal(t *testing.T) {
	reads, downs := acceptedReads(t)
	read := func(at int) func(*fakeOrca, []string) []string {
		return func(_ *fakeOrca, s []string) []string {
			s[at] = otherTerminal(s[at])
			return s
		}
	}
	answer := func(command, out string) func(*fakeOrca, []string) []string {
		return func(f *fakeOrca, s []string) []string {
			f.replies[command] = nil
			f.on(command, out)
			return s
		}
	}
	const (
		otherRead = `orca terminal read on task-a answered for terminal "term-2", not term-1`
		otherSend = `orca terminal send on task-a answered for terminal "term-2", not term-1`
		otherWait = `orca terminal wait on task-a answered for terminal "term-2", not term-1`
	)
	tests := []struct {
		name   string
		edit   func(*fakeOrca, []string) []string
		want   string
		probes int
		sent   map[string]int
	}{
		{"prompt screen", read(0), otherRead, 0, map[string]int{sendEnter: 0}},
		{"blocked idle wait", func(f *fakeOrca, s []string) []string {
			f.replies[waitIdle] = nil
			f.on(waitIdle, otherTerminal(ok(blockedWait)))
			return append([]string{historyLost(t, codexBanner)}, s...)
		}, otherWait, 0, map[string]int{waitIdle: 1, sendEnter: 0}},
		{"first browser view", read(1), otherRead, 1, map[string]int{sendEnter: 1, sendDown: 0}},
		{"browser during enumeration", read(4), otherRead, 1, map[string]int{sendDown: 3, sendUp: 0, sendTrust: 0}},
		{"browser during reset", read(downs + 3), otherRead, 1, map[string]int{sendDown: downs, sendUp: 2, sendTrust: 0}},
		{"trusted browser", read(len(reads) - 1), otherRead, 1, map[string]int{sendTrust: 1, sendEscape: 0, waitIdle: 0}},
		{"Enter receipt", answer(sendEnter, otherTerminal(ok(accepted))), otherSend, 1, map[string]int{sendEnter: 1, sendDown: 0}},
		{"Down receipt", answer(sendDown, otherTerminal(ok(accepted))), otherSend, 1, map[string]int{sendDown: 1, sendUp: 0, sendTrust: 0}},
		{"Up receipt", answer(sendUp, otherTerminal(ok(accepted))), otherSend, 1, map[string]int{sendDown: downs, sendUp: 1, sendTrust: 0}},
		{"trust receipt", answer(sendTrust, otherTerminal(ok(accepted))), otherSend, 1, map[string]int{sendTrust: 1, sendEscape: 0}},
		{"Escape receipt", answer(sendEscape, otherTerminal(ok(accepted))), otherSend, 1, map[string]int{sendEscape: 1, waitIdle: 0}},
		{"idle wait terminal", answer(waitIdle, otherTerminal(ok(idleWait))), otherWait, 1, map[string]int{sendEscape: 1, waitIdle: 1}},
		{"idle wait condition", answer(waitIdle, ok(strings.Replace(idleWait, `"condition":"tui-idle"`, `"condition":"exit"`, 1))), `satisfied "exit", not tui-idle`, 1, map[string]int{sendEscape: 1, waitIdle: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := codexFake(t)
			for _, screen := range tt.edit(fake, slices.Clone(reads)) {
				fake.on(readScreen, screen)
			}
			probe := &probeRecorder{t: t, fake: fake, out: exactProbe}
			steps, err := runCodex(t, fake, probe.review(captainPins), refusalPoll)
			if err == nil || !strings.Contains(err.Error(), tt.want) || slices.Contains(steps, "hooks") {
				t.Fatalf("Bootstrap = %v, %v, want %q", steps, err, tt.want)
			}
			if probe.calls != tt.probes {
				t.Errorf("probes %d, want %d", probe.calls, tt.probes)
			}
			assertSent(t, fake, tt.sent)
		})
	}
}

func TestCodexReviewRequiresAnExplicitLimitedFlag(t *testing.T) {
	reads, _ := acceptedReads(t)
	for _, truncated := range []bool{true, false} {
		t.Run(fmt.Sprintf("explicit false with truncated %t", truncated), func(t *testing.T) {
			fake := codexFake(t)
			for _, screen := range reads {
				fake.on(readScreen, strings.Replace(screen, `"truncated":true`, fmt.Sprintf(`"truncated":%t`, truncated), 1))
			}
			probe := &probeRecorder{t: t, fake: fake, out: exactProbe}
			steps, err := runCodex(t, fake, probe.review(captainPins), bootstrapPoll)
			if err != nil || !slices.Equal(steps, []string{"hooks"}) {
				t.Fatalf("Bootstrap = %v, %v", steps, err)
			}
			assertSent(t, fake, map[string]int{sendTrust: 1, sendEscape: 1, waitIdle: 1})
		})
	}
	frames := []struct {
		name   string
		at     int
		probes int
		sent   map[string]int
	}{
		{"prompt", 0, 0, map[string]int{sendEnter: 0}},
		{"browser", 4, 1, map[string]int{sendDown: 3, sendUp: 0, sendTrust: 0}},
		{"trusted browser", len(reads) - 1, 1, map[string]int{sendTrust: 1, sendEscape: 0, waitIdle: 0}},
	}
	values := []struct {
		name, limited, want string
	}{
		{"omitted", ``, "without its limited flag"},
		{"null", `"limited":null,`, "without its limited flag"},
		{"true", `"limited":true,`, "limited screen"},
	}
	for _, frame := range frames {
		for _, value := range values {
			t.Run(frame.name+" "+value.name, func(t *testing.T) {
				edited := slices.Clone(reads)
				edited[frame.at] = strings.Replace(edited[frame.at], `"limited":false,`, value.limited, 1)
				fake := codexFake(t)
				for _, screen := range edited {
					fake.on(readScreen, screen)
				}
				probe := &probeRecorder{t: t, fake: fake, out: exactProbe}
				steps, err := runCodex(t, fake, probe.review(captainPins), refusalPoll)
				if err == nil || !strings.Contains(err.Error(), value.want) || slices.Contains(steps, "hooks") {
					t.Fatalf("Bootstrap = %v, %v, want %q", steps, err, value.want)
				}
				if probe.calls != frame.probes {
					t.Errorf("probes %d, want %d", probe.calls, frame.probes)
				}
				assertSent(t, fake, frame.sent)
			})
		}
	}
}

func TestCodexReviewStaysWithinThePollTimeout(t *testing.T) {
	reads, downs := acceptedReads(t)
	short := orca.Poll{Interval: time.Millisecond, Timeout: 500 * time.Millisecond}
	tests := []struct {
		name    string
		command string
		sent    map[string]int
	}{
		{"probe", "", map[string]int{sendEnter: 0}},
		{"Enter", sendEnter, map[string]int{sendEnter: 1, sendDown: 0}},
		{"Down", sendDown, map[string]int{sendDown: 1, sendUp: 0}},
		{"Up", sendUp, map[string]int{sendDown: downs, sendUp: 1, sendTrust: 0}},
		{"trust", sendTrust, map[string]int{sendTrust: 1, sendEscape: 0}},
		{"Escape", sendEscape, map[string]int{sendEscape: 1, waitIdle: 0}},
		{"idle wait", waitIdle, map[string]int{sendTrust: 1, sendEscape: 1, waitIdle: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			wait := &stall{t: t, budget: short.Timeout}
			fake := codexFake(t)
			for _, screen := range reads {
				fake.on(readScreen, screen)
			}
			probe := &probeRecorder{t: t, fake: fake, out: exactProbe}
			review := probe.review(captainPins)
			exec := review.Exec
			var probed time.Time
			review.Exec = func(ctx context.Context, argv []string) ([]byte, error) {
				probed, _ = ctx.Deadline()
				if tt.command == "" {
					probe.calls++
					return nil, wait.until(ctx)
				}
				return exec(ctx, argv)
			}
			runner := stallingOrca{fake: fake, stall: wait, command: tt.command}
			steps, err := orca.NewClient(runner).On(env, runtimeID).Bootstrap(ctx, "term-1", orca.CodexStartup(review), false, short)
			if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil || slices.Contains(steps, "hooks") {
				t.Fatalf("Bootstrap = %v, %v, want the review's own deadline", steps, err)
			}
			if probe.calls != 1 || !wait.deadline.Equal(probed) {
				t.Errorf("probes %d; the stalled call ran until %v, not the review's deadline %v", probe.calls, wait.deadline, probed)
			}
			assertSent(t, fake, tt.sent)
		})
	}
}

func TestParseHookProbeReturnsOnlySafeFields(t *testing.T) {
	probe, err := orca.ParseHookProbe([]byte(exactProbe+"\n"), captainPins[0])
	if err != nil || probe.Home != probeHome || probe.Root != probeRoot || probe.Definitions != 13 || probe.Events["Stop"] != 2 {
		t.Fatalf("ParseHookProbe = %+v, %v", probe, err)
	}
	tests := []struct {
		name, out, want string
	}{
		{"unknown field", strings.Replace(exactProbe, `"schema":1`, `"schema":1,"command":"`+leaked+`"`, 1), "no single result document"},
		{"trailing document", exactProbe + exactProbe, "no single result document"},
		{"other schema", strings.Replace(exactProbe, `"schema":1`, `"schema":2`, 1), "another schema"},
		{"unrecognized reason", `{"schema":1,"outcome":"refused","reason":"` + leaked + `","event":"","record":-1}`, "unrecognized reason"},
		{"unrecognized event", `{"schema":1,"outcome":"refused","reason":"mismatch","event":"` + leaked + `","record":0}`, "unrecognized reason"},
		{"unrecognized outcome", strings.Replace(exactProbe, `"exact"`, `"approved"`, 1), "unrecognized outcome"},
		{"relative home", strings.Replace(exactProbe, `"home":"/home/agent"`, `"home":"home/agent"`, 1), "no absolute runtime home"},
		{"no digest", strings.Replace(exactProbe, probeDigest, "abc", 1), "no hooks digest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := orca.ParseHookProbe([]byte(tt.out), captainPins[0])
			if err == nil || !strings.Contains(err.Error(), tt.want) || strings.Contains(err.Error(), leaked) {
				t.Errorf("ParseHookProbe = %v, want %q without the probe's values", err, tt.want)
			}
		})
	}
	var refusal orca.HookRefusal
	_, err = orca.ParseHookProbe([]byte(`{"schema":1,"outcome":"refused","reason":"missing","event":"PreToolUse","record":1}`), captainPins[0])
	if !errors.As(err, &refusal) || refusal != (orca.HookRefusal{Reason: "missing", Event: "PreToolUse", Record: 1}) {
		t.Errorf("refusal = %+v, %v", refusal, err)
	}
}
