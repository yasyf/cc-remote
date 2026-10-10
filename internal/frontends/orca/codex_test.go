package orca_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/yasyf/cc-remote/internal/frontends/orca"
)

const (
	sendTrust   = "terminal send --terminal term-1 --text t" + scope
	sendEnd     = "terminal send --terminal term-1 --text \x1b[F" + scope
	sendHome    = "terminal send --terminal term-1 --text \x1b[H" + scope
	sendEscape  = "terminal send --terminal term-1 --text " + orca.KeyEscape + scope
	idleWait    = `{"wait":{"handle":"term-1","condition":"tui-idle","satisfied":true,"status":"running","exitCode":null}}`
	blockedWait = `{"wait":{"handle":"term-1","condition":"tui-idle","satisfied":false,"status":"running","exitCode":null,"blockedReason":"agent-hooks-review-prompt"}}`
	trustWait   = `{"wait":{"handle":"term-1","condition":"tui-idle","satisfied":false,"status":"running","exitCode":null,"blockedReason":"agent-trust-workspace"}}`
	trustChoice = "1. Trust and continue"
	quitChoice  = "2. Quit"
	probeHome   = "/home/agent"
	probeRoot   = probeHome + "/.claude/plugins/cache/captain-hook/captain-hook/12.79.15"
	probeDigest = "1111111111111111111111111111111111111111111111111111111111111111"
	leaked      = "sk-synthetic-credential-0000"
)

var (
	captainPins  = []orca.Pin{{ID: "captain-hook@captain-hook", Version: "12.79.15"}}
	exactProbe   = `{"schema":1,"outcome":"exact","reason":"","event":"","record":-1,"home":"` + probeHome + `","root":"` + probeRoot + `","hooksSha256":"` + probeDigest + `","definitions":13,"events":{"SessionStart":2,"UserPromptSubmit":2,"PreToolUse":2,"PermissionRequest":1,"PostToolUse":2,"SubagentStart":1,"SubagentStop":1,"Stop":2}}`
	managedProbe = `{"schema":1,"outcome":"exact","reason":"","event":"","record":-1,"home":"` + probeHome + `","root":"` + probeRoot + `","hooksSha256":"` + probeDigest + `","definitions":5,"events":{"SessionStart":1,"UserPromptSubmit":1,"PreToolUse":1,"PostToolUse":1,"Stop":1}}`
	codexBanner  = []string{"  >_ OpenAI Codex (v0.159.2)", "     ~/app", "  permissions: YOLO mode"}
	codexReady   = append(slices.Clone(codexBanner), "  › Ask Codex anything")
	refusalPoll  = orca.Poll{Interval: time.Millisecond, Timeout: time.Second}
	folderShown  = []string{
		"  Folder access",
		"  /workspaces/monorepo",
		"  Trust this folder? Codex can read, edit, and run files here, subject to your permission settings. Folder settings",
		"  can run code automatically, even without a model request. Continue only if you trust these files. Your trust",
		"  decision will be saved.",
		"› 1. Trust and continue",
		"  2. Quit",
		"  enter continue · esc quit",
	}
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

var managedRows = []browserRow{
	{"PreToolUse", 1, 0, 1, "Before a tool executes"},
	{"PermissionRequest", 0, 0, 0, "When permission is requested"},
	{"PostToolUse", 1, 0, 1, "After a tool executes"},
	{"PreCompact", 0, 0, 0, "Before context compaction"},
	{"PostCompact", 0, 0, 0, "After context compaction"},
	{"SessionStart", 1, 0, 1, "When a new session starts"},
	{"SessionEnd", 0, 0, 0, "Right before a session ends"},
	{"UserPromptSubmit", 1, 0, 1, "When the user submits a prompt"},
	{"SubagentStart", 0, 0, 0, "When a subagent is created"},
	{"SubagentStop", 0, 0, 0, "Right before a subagent ends its turn"},
	{"Stop", 1, 0, 1, "Right before Codex ends its turn"},
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

func folderAccess(selected string) []string {
	lines := slices.Clone(folderShown[:5])
	for _, option := range []string{trustChoice, quitChoice} {
		marker := "  "
		if option == selected {
			marker = "› "
		}
		lines = append(lines, marker+option)
	}
	return append(lines, folderShown[7])
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

func trustedScreen(rows []browserRow, top, selected, window int) []string {
	lines := slices.Clone(codexBanner)
	lines = append(lines, "  Hooks", "  Lifecycle hooks from config and enabled plugins.", "  Event                 Installed   Active      Description")
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
		lines = append(lines, fmt.Sprintf("%s %-21s %-11d %-11d %s", marker, row.event, row.installed, row.active, row.description))
	}
	if end < len(rows) {
		lines = append(lines, "↓")
	}
	return append(lines, "  enter details · esc close")
}

func trusted(rows []browserRow) []browserRow {
	active := slices.Clone(rows)
	for i := range active {
		active[i].active, active[i].review = active[i].installed, 0
	}
	return active
}

func firstEdge(rows []browserRow, window, pending int) []string {
	return browserScreen(rows, 0, 0, window, pending, window < len(rows))
}

func lastEdge(rows []browserRow, window, pending int) []string {
	return browserScreen(rows, max(len(rows)-window, 0), len(rows)-1, window, pending, false)
}

func trustedLast(rows []browserRow, window int) []string {
	return trustedScreen(rows, max(len(rows)-window, 0), len(rows)-1, window)
}

func reviewReads(t *testing.T, rows []browserRow, window, pending int) []string {
	t.Helper()
	return []string{historyLost(t, firstEdge(rows, window, pending)), historyLost(t, lastEdge(rows, window, pending))}
}

func trustedReads(t *testing.T, rows []browserRow, window int, redrawn bool) []string {
	t.Helper()
	first, last := historyLost(t, trustedScreen(rows, 0, 0, window)), historyLost(t, trustedLast(rows, window))
	if redrawn {
		return []string{first, last}
	}
	return []string{last, first}
}

type probeRecorder struct {
	t      *testing.T
	fake   *fakeOrca
	out    string
	calls  int
	enters int
}

func (p *probeRecorder) review(captain []orca.Pin) orca.HookReview {
	return p.reviewOn(orca.ServerElectron, captain)
}

func (p *probeRecorder) reviewOn(server orca.Server, captain []orca.Pin) orca.HookReview {
	return orca.HookReview{Captain: captain, Server: server, Exec: func(_ context.Context, argv []string) ([]byte, error) {
		p.calls++
		if p.fake.called(sendEnter) != p.enters || p.fake.called(sendTrust) != 0 {
			p.t.Error("the probe ran after review input")
		}
		if len(argv) != 6 || argv[0] != "python3" || argv[1] != "-c" || argv[2] != orca.HookProbeScript || argv[3] != captain[0].ID || argv[4] != captain[0].Version || argv[5] != string(server) {
			p.t.Errorf("probe argv = %q", argv)
		}
		return []byte(p.out), nil
	}}
}

func codexFake(t *testing.T) *fakeOrca {
	return newFakeOrca(t).on(sendEnter, ok(accepted)).on(sendUp, ok(accepted)).on(sendEnd, ok(accepted)).on(sendHome, ok(accepted)).
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

func runTrustedCodex(t *testing.T, fake *fakeOrca, review orca.HookReview, p orca.Poll) ([]string, error) {
	t.Helper()
	return orca.NewClient(fake).On(env, runtimeID).Bootstrap(context.Background(), "term-1", orca.CodexStartup(review), true, p)
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

func acceptedReads(t *testing.T) []string {
	t.Helper()
	return slices.Concat([]string{historyLost(t, promptScreen(5, ""))}, reviewReads(t, acceptedRows, 8, 5), trustedReads(t, trusted(acceptedRows), 8, false))
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

func TestCodexReviewCoversBothEdgesAndTrustsOnce(t *testing.T) {
	tests := []struct {
		name    string
		redrawn bool
		edge    string
		sent    map[string]int
	}{
		{"trusted view stays at the last edge", false, sendHome, map[string]int{sendEnd: 1, sendHome: 1}},
		{"trusted view redraws at the first edge", true, sendEnd, map[string]int{sendEnd: 2, sendHome: 0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				logs := recordTimings(t)
				fake := codexFake(t).on(readScreen, historyLost(t, promptScreen(5, "")))
				for _, screen := range slices.Concat(reviewReads(t, acceptedRows, 8, 5), trustedReads(t, trusted(acceptedRows), 8, tt.redrawn)) {
					fake.on(readScreen, screen)
				}
				probe := &probeRecorder{t: t, fake: fake, out: exactProbe}
				review := probe.review(captainPins)
				exec := review.Exec
				review.Exec = func(ctx context.Context, argv []string) ([]byte, error) {
					time.Sleep(500 * time.Millisecond)
					return exec(ctx, argv)
				}
				steps, err := runCodex(t, fake, review, bootstrapPoll)
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(steps, []string{"hooks"}) || probe.calls != 1 {
					t.Fatalf("steps %v, probes %d", steps, probe.calls)
				}
				assertSent(t, fake, map[string]int{sendEnter: 1, sendDown: 0, sendUp: 0, sendTrust: 1, sendEscape: 1, waitIdle: 1, readScreen: 5})
				assertSent(t, fake, tt.sent)
				enter, end, trust := position(fake, sendEnter, 0), position(fake, sendEnd, 0), position(fake, sendTrust, 0)
				edge, escape, idle := lastPosition(fake, tt.edge), position(fake, sendEscape, 0), position(fake, waitIdle, 0)
				if enter >= end || end >= trust || trust >= edge || edge >= escape || escape >= idle {
					t.Errorf("order enter %d, End %d, trust %d, edge %d, escape %d, idle %d", enter, end, trust, edge, escape, idle)
				}
				for i, input := range []int{end, trust, edge, escape} {
					if position(fake, readScreen, i+1) > input {
						t.Errorf("input %d at %d was sent before read %d observed its frame", i, input, i+1)
					}
				}
				read, key := "remote.screen ok=true seconds=0", "remote.key ok=true seconds=0"
				want := slices.Concat(
					[]string{read, "hooks.probe ok=true seconds=0.5", "remote.enter ok=true seconds=0", read},
					slices.Repeat([]string{key, read}, 3),
					[]string{key, "remote.waitIdle ok=true seconds=0"},
					[]string{"hooks.review moves=2 ok=true seconds=0.5", "bootstrap ok=true seconds=0.5 steps=1"},
				)
				if got := timings(t, logs, probeHome, probeDigest, captainPins[0].ID, captainPins[0].Version, "PreToolUse"); !slices.Equal(got, want) {
					t.Errorf("timings =\n%q\nwant\n%q", got, want)
				}
			})
		})
	}
}

func TestCodexReviewAcceptsTheSetOfTheServerOrcaReports(t *testing.T) {
	refused := map[string]int{sendEnter: 0, sendTrust: 0, sendEscape: 0}
	browsed := map[string]int{sendEnter: 1, sendTrust: 0, sendEscape: 0}
	tests := []struct {
		name   string
		server orca.Server
		rows   []browserRow
		probe  string
		want   string
		sent   map[string]int
	}{
		{"managed server with only Captain Hook", orca.ServerManaged, managedRows, managedProbe, "", map[string]int{sendEnter: 1, sendTrust: 1, sendEscape: 1, waitIdle: 1}},
		{"managed server with Orca's handlers in the hooks file", orca.ServerManaged, managedRows, exactProbe, "another set of definitions", refused},
		{"managed server with Orca's handlers in the browser", orca.ServerManaged, acceptedRows, managedProbe, "PreToolUse with 2 installed, 1 active and 1 to review, not 1, 0 and 1", browsed},
		{"Electron without Orca's handlers in the hooks file", orca.ServerElectron, acceptedRows, managedProbe, "another set of definitions", refused},
		{"Electron without Orca's handlers in the browser", orca.ServerElectron, managedRows, exactProbe, "PreToolUse with 1 installed, 0 active and 1 to review, not 2, 1 and 1", browsed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := codexFake(t).on(readScreen, historyLost(t, promptScreen(5, "")))
			for _, screen := range slices.Concat(reviewReads(t, tt.rows, 8, 5), trustedReads(t, trusted(tt.rows), 8, false)) {
				fake.on(readScreen, screen)
			}
			probe := &probeRecorder{t: t, fake: fake, out: tt.probe}
			steps, err := runCodex(t, fake, probe.reviewOn(tt.server, captainPins), refusalPoll)
			switch {
			case tt.want == "" && (err != nil || !slices.Equal(steps, []string{"hooks"})):
				t.Fatalf("Bootstrap = %v, %v", steps, err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Fatalf("Bootstrap = %v, want %q", err, tt.want)
			}
			if probe.calls != 1 {
				t.Errorf("probes %d", probe.calls)
			}
			assertSent(t, fake, tt.sent)
		})
	}
}

func TestCodexReviewRequiresOverlappingEdges(t *testing.T) {
	tests := []struct {
		name   string
		window int
		after  int
		want   string
		sent   map[string]int
	}{
		{"seven rows overlap by two", 7, 7, "", map[string]int{sendEnd: 1, sendHome: 1, sendTrust: 1, sendEscape: 1, waitIdle: 1}},
		{"six rows only touch", 6, 6, "showed a new row out of order", map[string]int{sendEnd: 1, sendHome: 0, sendTrust: 0, sendEscape: 0}},
		{"four rows leave a gap", 4, 4, "showed a new row out of order", map[string]int{sendEnd: 1, sendHome: 0, sendTrust: 0, sendEscape: 0}},
		{"trusted rows only touch", 8, 6, "showed a new row out of order", map[string]int{sendEnd: 1, sendHome: 1, sendTrust: 1, sendEscape: 0, waitIdle: 0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := codexFake(t).on(readScreen, historyLost(t, promptScreen(5, "1. Review hooks")))
			for _, screen := range slices.Concat(reviewReads(t, acceptedRows, tt.window, 5), trustedReads(t, trusted(acceptedRows), tt.after, false)) {
				fake.on(readScreen, screen)
			}
			probe := &probeRecorder{t: t, fake: fake, out: exactProbe}
			_, err := runCodex(t, fake, probe.review(captainPins), refusalPoll)
			if tt.want == "" && err != nil || tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Fatalf("Bootstrap = %v, want %q", err, tt.want)
			}
			assertSent(t, fake, tt.sent)
		})
	}
}

func TestCodexAlreadyTrustedSessionSendsNoTrustInput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logs := recordTimings(t)
		fake := codexFake(t).on(readScreen, historyLost(t, append(slices.Clone(codexBanner), "  › Ask Codex anything")))
		probe := &probeRecorder{t: t, fake: fake, out: exactProbe}
		steps, err := runCodex(t, fake, probe.review(captainPins), bootstrapPoll)
		if err != nil || len(steps) != 0 || probe.calls != 0 || fake.called(waitIdle) != 1 {
			t.Fatalf("steps %v, err %v, probes %d", steps, err, probe.calls)
		}
		for _, command := range []string{sendEnter, sendDown, sendUp, sendEnd, sendHome, sendTrust, sendEscape} {
			if fake.called(command) != 0 {
				t.Errorf("an already trusted session sent %q", command)
			}
		}
		want := []string{"remote.screen ok=true seconds=0", "remote.waitIdle ok=true seconds=0", "bootstrap ok=true seconds=0 steps=0"}
		if got := timings(t, logs); !slices.Equal(got, want) {
			t.Errorf("timings = %q, want %q", got, want)
		}
	})
}

func TestCodexReviewStartsAfterTheIdleWaitReportsThePrompt(t *testing.T) {
	fake := newFakeOrca(t).on(sendEnter, ok(accepted)).on(sendEnd, ok(accepted)).on(sendHome, ok(accepted)).
		on(sendTrust, ok(accepted)).on(sendEscape, ok(accepted)).on(waitIdle, ok(blockedWait)).on(waitIdle, ok(idleWait)).
		on(readScreen, historyLost(t, codexBanner)).on(readScreen, historyLost(t, promptScreen(5, "")))
	for _, screen := range slices.Concat(reviewReads(t, acceptedRows, 8, 5), trustedReads(t, trusted(acceptedRows), 8, false)) {
		fake.on(readScreen, screen)
	}
	probe := &probeRecorder{t: t, fake: fake, out: exactProbe}
	if _, err := runCodex(t, fake, probe.review(captainPins), bootstrapPoll); err != nil {
		t.Fatal(err)
	}
	if fake.called(waitIdle) != 2 || fake.called(sendTrust) != 1 || position(fake, waitIdle, 0) > position(fake, sendEnter, 0) {
		t.Errorf("waits %d, trust %d", fake.called(waitIdle), fake.called(sendTrust))
	}
}

func TestCodexTrustsTheFolderBeforeReviewingHooks(t *testing.T) {
	reads := acceptedReads(t)
	tests := []struct {
		name  string
		early []string
		waits int
	}{
		{"folder screen first", nil, 1},
		{"after the idle wait reports it", []string{historyLost(t, []string{""})}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeOrca(t).on(sendEnter, ok(accepted)).on(sendEnd, ok(accepted)).on(sendHome, ok(accepted)).
				on(sendTrust, ok(accepted)).on(sendEscape, ok(accepted))
			if tt.early != nil {
				fake.on(waitIdle, ok(trustWait))
			}
			fake.on(waitIdle, ok(idleWait))
			for _, screen := range slices.Concat(tt.early, []string{historyLost(t, folderShown), reads[0]}, reads) {
				fake.on(readScreen, screen)
			}
			probe := &probeRecorder{t: t, fake: fake, out: exactProbe, enters: 1}
			steps, err := runTrustedCodex(t, fake, probe.review(captainPins), bootstrapPoll)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(steps, []string{"trust", "hooks"}) || probe.calls != 1 {
				t.Fatalf("steps %v, probes %d", steps, probe.calls)
			}
			assertSent(t, fake, map[string]int{sendEnter: 2, sendDown: 0, sendUp: 0, sendEnd: 1, sendHome: 1, sendTrust: 1, sendEscape: 1, waitIdle: tt.waits})
			folder, review := position(fake, sendEnter, 0), position(fake, sendEnter, 1)
			if folder >= review || review >= position(fake, sendEnd, 0) || lastPosition(fake, sendEscape) >= lastPosition(fake, waitIdle) {
				t.Errorf("order folder Enter %d, review Enter %d, End %d", folder, review, position(fake, sendEnd, 0))
			}
			if tt.early != nil && position(fake, waitIdle, 0) >= folder {
				t.Error("the folder was trusted before the idle wait reported it")
			}
		})
	}
}

func TestCodexFolderTrustRefusesAnUnlistedOwnerBeforeAnyInput(t *testing.T) {
	tests := []struct {
		name  string
		reads [][]string
		waits []string
	}{
		{"folder screen", [][]string{folderShown}, nil},
		{"after the idle wait reports it", [][]string{{""}, folderShown}, []string{trustWait}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeOrca(t)
			for _, screen := range tt.reads {
				fake.on(readScreen, historyLost(t, screen))
			}
			for _, wait := range tt.waits {
				fake.on(waitIdle, ok(wait))
			}
			probe := &probeRecorder{t: t, fake: fake, out: exactProbe}
			steps, err := runCodex(t, fake, probe.review(captainPins), refusalPoll)
			if !errors.Is(err, orca.ErrUntrusted) || len(steps) != 0 || probe.calls != 0 {
				t.Fatalf("Bootstrap = %v, %v, probes %d, want ErrUntrusted before any probe", steps, err, probe.calls)
			}
			assertSent(t, fake, map[string]int{sendEnter: 0, sendUp: 0, sendDown: 0, sendTrust: 0, sendEscape: 0, waitIdle: len(tt.waits)})
		})
	}
}

func TestCodexFolderTrustEntersOnlyTheVerifiedChoice(t *testing.T) {
	other := slices.Replace(folderAccess(""), 5, 6, "› 3. Something else")
	tests := []struct {
		name   string
		reads  [][]string
		steps  []string
		want   string
		ups    int
		enters int
	}{
		{"already ready", [][]string{codexReady}, nil, "", 0, 0},
		{"trust selected", [][]string{folderShown, codexReady}, []string{"trust"}, "", 0, 1},
		{"quit selected", [][]string{folderAccess(quitChoice), folderAccess(trustChoice), codexReady}, []string{"trust"}, "", 1, 1},
		{"never reaches trust", [][]string{folderAccess(quitChoice), other, folderAccess(quitChoice), other, folderAccess(quitChoice)}, nil, "trust: 4 moves never selected", 4, 0},
		{"no selection", [][]string{folderAccess("")}, nil, "matched no known startup state", 1, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := codexFake(t)
			for _, screen := range tt.reads {
				fake.on(readScreen, historyLost(t, screen))
			}
			probe := &probeRecorder{t: t, fake: fake, out: exactProbe}
			steps, err := runTrustedCodex(t, fake, probe.review(captainPins), refusalPoll)
			if tt.want == "" && err != nil || tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Fatalf("Bootstrap = %v, want %q", err, tt.want)
			}
			if !slices.Equal(steps, tt.steps) || probe.calls != 0 {
				t.Errorf("steps %v (want %v), probes %d", steps, tt.steps, probe.calls)
			}
			idle := 0
			if tt.want == "" {
				idle = 1
			}
			assertSent(t, fake, map[string]int{sendUp: tt.ups, sendEnter: tt.enters, sendDown: 0, sendTrust: 0, sendEscape: 0, waitIdle: idle})
			if tt.ups > 0 && tt.enters > 0 && lastPosition(fake, sendUp) >= position(fake, sendEnter, 0) {
				t.Error("Enter was sent before the selection was verified")
			}
		})
	}
}

func TestCodexFolderTrustNeverResendsEnter(t *testing.T) {
	folder := historyLost(t, folderShown)
	tests := []struct {
		name   string
		read   string
		enter  func(*fakeOrca)
		want   string
		enters int
	}{
		{"folder screen for another terminal", otherTerminal(folder), nil, `orca terminal read on task-a answered for terminal "term-2", not term-1`, 0},
		{"folder screen from another runtime", strings.Replace(folder, `"runtimeId":"`+runtimeID+`"`, `"runtimeId":"rt-2"`, 1), nil, `answered from runtime "rt-2"`, 0},
		{"Enter receipt for another terminal", folder, func(f *fakeOrca) { f.on(sendEnter, otherTerminal(ok(accepted))) }, `trust: orca terminal send on task-a answered for terminal "term-2", not term-1`, 1},
		{"ambiguous Enter", folder, func(f *fakeOrca) { f.fail(sendEnter, "", errors.New("connection reset")) }, "trust: connection reset", 1},
		{"screen stays after Enter", folder, func(f *fakeOrca) { f.on(sendEnter, ok(accepted)) }, "matched no known startup state", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeOrca(t).on(readScreen, tt.read)
			if tt.enter != nil {
				tt.enter(fake)
			}
			probe := &probeRecorder{t: t, fake: fake, out: exactProbe}
			steps, err := runTrustedCodex(t, fake, probe.review(captainPins), refusalPoll)
			if err == nil || !strings.Contains(err.Error(), tt.want) || len(steps) != 0 {
				t.Fatalf("Bootstrap = %v, %v, want %q", steps, err, tt.want)
			}
			if probe.calls != 0 {
				t.Errorf("probes %d after a failed folder trust", probe.calls)
			}
			assertSent(t, fake, map[string]int{sendEnter: tt.enters, sendUp: 0, sendDown: 0, sendTrust: 0, sendEscape: 0, waitIdle: 0})
		})
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
			synctest.Test(t, func(t *testing.T) {
				logs := recordTimings(t)
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
				got := timings(t, logs, probeHome, probeDigest, captainPins[0].ID, captainPins[0].Version, "/home/sprite", "mismatch", "Stop")
				probes, reviews := 0, 0
				for _, line := range got {
					switch {
					case line == "hooks.probe ok=true seconds=0":
						probes++
					case line == "hooks.review moves=0 ok=false seconds=0":
						reviews++
					case strings.HasPrefix(line, "hooks."), strings.HasPrefix(line, "remote.key"), strings.HasPrefix(line, "remote.enter"):
						t.Errorf("a refused review recorded %q", line)
					}
				}
				if probes != tt.probes || reviews > 1 || probes > reviews || len(got) == 0 || got[len(got)-1] != "bootstrap ok=false seconds=0 steps=0" {
					t.Errorf("timings = %q, want %d probes and a refused bootstrap", got, tt.probes)
				}
			})
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
	changed := slices.Clone(acceptedRows)
	changed[5] = browserRow{"SessionStart", 2, 2, 0, "When a new session starts"}
	swapped := slices.Clone(acceptedRows)
	swapped[5], swapped[6] = swapped[6], swapped[5]
	duplicated := slices.Clone(acceptedRows)
	duplicated[10] = acceptedRows[9]
	first := historyLost(t, firstEdge(acceptedRows, 8, 5))
	detail := historyLost(t, slices.Concat(codexBanner, []string{"  PreToolUse hooks", "  1 hook needs review before it can run.", "› [!] Hook 1 · new", "  [x] Hook 2", "  t trust · esc back"}))
	tests := []struct {
		name  string
		reads []string
		want  string
		ends  int
	}{
		{"missing row", reviewReads(t, without("SubagentStop"), 8, 5), "lists no SubagentStop row", 1},
		{"missing PreCompact row", reviewReads(t, without("PreCompact"), 8, 5), "lists no PreCompact row", 1},
		{"missing PostCompact row", reviewReads(t, without("PostCompact"), 8, 5), "lists no PostCompact row", 1},
		{"missing SessionEnd row", reviewReads(t, without("SessionEnd"), 8, 5), "lists no SessionEnd row", 1},
		{"missing Interrupt row", reviewReads(t, without("Interrupt"), 8, 5), "moved from PreToolUse to Stop, not its other edge", 1},
		{"guessed row order", reviewReads(t, guessed, 8, 5), "out of the supported order", 1},
		{"extra nonzero row", reviewReads(t, extra, 8, 5), "PreCompact with 1 installed", 1},
		{"unknown last row", reviewReads(t, unknown, 8, 5), "moved from PreToolUse to Notification, not its other edge", 1},
		{"unknown overlap row", []string{first, historyLost(t, browserScreen(slices.Concat(acceptedRows[:4], unknown[11:], acceptedRows[5:]), 4, 11, 8, 5, false))}, "unsupported event row", 1},
		{"old pilot rows", reviewReads(t, pilot, 8, 5), "PreToolUse with 3 installed", 1},
		{"browser review total", reviewReads(t, acceptedRows, 8, 10), "counts 10 hooks to review, not 5", 0},
		{"last edge review total", []string{first, historyLost(t, lastEdge(acceptedRows, 8, 4))}, "counts 4 hooks to review, not 5", 1},
		{"counts change between edges", []string{first, historyLost(t, lastEdge(changed, 8, 5))}, "changed the SessionStart counts", 1},
		{"order changes between edges", []string{first, historyLost(t, lastEdge(swapped, 8, 5))}, "changed its row order between views", 1},
		{"duplicate row at the last edge", []string{first, historyLost(t, lastEdge(duplicated, 8, 5))}, "lists an event twice", 1},
		{"last edge keeps a lower marker", []string{first, historyLost(t, browserScreen(acceptedRows, 4, 11, 8, 5, true))}, "moved from PreToolUse to Interrupt, not its other edge", 1},
		{"last edge selects another row", []string{first, historyLost(t, browserScreen(acceptedRows, 4, 10, 8, 5, false))}, "moved from PreToolUse to Stop, not its other edge", 1},
		{"End opens a detail view", []string{first, detail}, "not the recognized hooks browser", 1},
		{"End shows the trusted layout", []string{first, historyLost(t, trustedLast(trusted(acceptedRows), 8))}, "not the recognized hooks browser", 1},
		{"limited last edge", []string{first, frameOf(t, "screen", false, true, lastEdge(acceptedRows, 8, 5))}, "limited screen", 1},
		{"first view selects another row", []string{historyLost(t, browserScreen(acceptedRows, 0, 1, 8, 5, true))}, "did not open at its first row", 0},
		{"first view shows an upper marker", []string{historyLost(t, browserScreen(acceptedRows, 1, 1, 8, 5, true))}, "did not open at its first row", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fake := codexFake(t).on(readScreen, historyLost(t, promptScreen(5, "")))
				for _, screen := range tt.reads {
					fake.on(readScreen, screen)
				}
				probe := &probeRecorder{t: t, fake: fake, out: exactProbe}
				_, err := runCodex(t, fake, probe.review(captainPins), refusalPoll)
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("Bootstrap = %v, want %q", err, tt.want)
				}
				assertSent(t, fake, map[string]int{sendEnd: tt.ends, sendHome: 0, sendDown: 0, sendUp: 0})
				assertNoTrust(t, fake)
			})
		})
	}
}

func TestCodexReviewStopsOnStuckOrUnverifiedEdges(t *testing.T) {
	reads := acceptedReads(t)
	active := trusted(acceptedRows)
	tests := []struct {
		name   string
		edit   func(*fakeOrca, []string) []string
		want   string
		sent   map[string]int
		review string
	}{
		{"stuck End", func(_ *fakeOrca, s []string) []string { return s[:2] }, "selection never moved from PreToolUse", map[string]int{sendEnd: 1, sendTrust: 0}, "hooks.review moves=0 ok=false seconds=1"},
		{"End stops short", func(_ *fakeOrca, s []string) []string {
			return append(slices.Clone(s[:2]), historyLost(t, browserScreen(acceptedRows, 0, 7, 8, 5, true)))
		}, "moved from PreToolUse to UserPromptSubmit, not its other edge", map[string]int{sendEnd: 1, sendTrust: 0}, "hooks.review moves=0 ok=false seconds=0"},
		{"ambiguous End", func(f *fakeOrca, s []string) []string {
			f.replies[sendEnd] = nil
			f.fail(sendEnd, "", errors.New("connection reset"))
			return s
		}, "connection reset", map[string]int{sendEnd: 1, sendTrust: 0}, "hooks.review moves=0 ok=false seconds=0"},
		{"stuck trusted Home", func(_ *fakeOrca, s []string) []string { return s[:4] }, "selection never moved from Interrupt", map[string]int{sendEnd: 1, sendTrust: 1, sendHome: 1}, "hooks.review moves=1 ok=false seconds=1"},
		{"trusted Home stops short", func(_ *fakeOrca, s []string) []string {
			return append(slices.Clone(s[:4]), historyLost(t, trustedScreen(active, 3, 8, 8)))
		}, "moved from Interrupt to SubagentStart, not its other edge", map[string]int{sendEnd: 1, sendTrust: 1, sendHome: 1}, "hooks.review moves=1 ok=false seconds=0"},
		{"ambiguous trusted Home", func(f *fakeOrca, s []string) []string {
			f.replies[sendHome] = nil
			f.fail(sendHome, "", errors.New("connection reset"))
			return s
		}, "connection reset", map[string]int{sendEnd: 1, sendTrust: 1, sendHome: 1}, "hooks.review moves=1 ok=false seconds=0"},
		{"stuck trusted End", func(_ *fakeOrca, s []string) []string {
			return append(slices.Clone(s[:3]), historyLost(t, trustedScreen(active, 0, 0, 8)))
		}, "selection never moved from PreToolUse", map[string]int{sendEnd: 2, sendTrust: 1, sendHome: 0}, "hooks.review moves=1 ok=false seconds=1"},
		{"trusted view at neither edge", func(_ *fakeOrca, s []string) []string {
			return append(slices.Clone(s[:3]), historyLost(t, trustedScreen(active, 2, 5, 8)))
		}, "selects SessionStart, at neither edge of its events", map[string]int{sendEnd: 1, sendTrust: 1, sendHome: 0}, "hooks.review moves=1 ok=false seconds=0"},
		{"trusted Home opens another view", func(_ *fakeOrca, s []string) []string {
			return append(slices.Clone(s[:4]), historyLost(t, codexReady))
		}, "not the recognized hooks browser", map[string]int{sendEnd: 1, sendTrust: 1, sendHome: 1}, "hooks.review moves=1 ok=false seconds=1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				logs := recordTimings(t)
				fake := codexFake(t)
				for _, screen := range tt.edit(fake, slices.Clone(reads)) {
					fake.on(readScreen, screen)
				}
				probe := &probeRecorder{t: t, fake: fake, out: exactProbe}
				_, err := runCodex(t, fake, probe.review(captainPins), refusalPoll)
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("Bootstrap = %v, want %q", err, tt.want)
				}
				assertSent(t, fake, tt.sent)
				assertSent(t, fake, map[string]int{sendDown: 0, sendUp: 0, sendEscape: 0, waitIdle: 0})
				reviews := slices.DeleteFunc(timings(t, logs, "connection reset", "Interrupt"), func(line string) bool { return !strings.HasPrefix(line, "hooks.review ") })
				if !slices.Equal(reviews, []string{tt.review}) {
					t.Errorf("review timings = %q, want %q", reviews, tt.review)
				}
			})
		})
	}
}

func TestCodexTrustIsSentOnceAndVerifiedAtBothEdges(t *testing.T) {
	reads := acceptedReads(t)
	active := trusted(acceptedRows)
	edited := func(event string, installed, enabled int) []browserRow {
		rows := slices.Clone(active)
		i := slices.IndexFunc(rows, func(r browserRow) bool { return r.event == event })
		rows[i].installed, rows[i].active = installed, enabled
		return rows
	}
	residual := slices.Clone(acceptedRows)
	for _, i := range []int{0, 2, 5, 7} {
		residual[i].active, residual[i].review = 2, 0
	}
	unknown := append(slices.Clone(active[:11]), browserRow{"Notification", 0, 0, 0, "Synthetic"})
	swapped := slices.Clone(active)
	swapped[8], swapped[9] = swapped[9], swapped[8]
	detail := historyLost(t, slices.Concat(codexBanner, []string{"  PreToolUse hooks", "  Turn hooks on or off. Your changes are saved automatically.", "› [x] Hook 1", "  [x] Hook 2", "  space/enter toggle · esc back"}))
	tests := []struct {
		name  string
		edit  func(*fakeOrca)
		after []string
		want  string
		homes int
	}{
		{"ambiguous trust", func(f *fakeOrca) {
			f.replies[sendTrust] = nil
			f.fail(sendTrust, "", errors.New("connection reset"))
		}, reads[3:], "connection reset", 0},
		{"trust never lands", func(*fakeOrca) {}, reads[2:3], "did not produce the trusted hooks browser", 0},
		{"residual review state", func(*fakeOrca) {}, []string{historyLost(t, lastEdge(residual, 8, 1))}, "did not produce the trusted hooks browser", 0},
		{"trust opens a detail view", func(*fakeOrca) {}, []string{detail}, "did not produce the trusted hooks browser", 0},
		{"disabled handler at the last edge", func(*fakeOrca) {}, []string{historyLost(t, trustedLast(edited("Stop", 2, 1), 8)), reads[4]}, "shows 1 of 2 Stop hooks active", 1},
		{"disabled handler at the first edge", func(*fakeOrca) {}, []string{reads[3], historyLost(t, trustedScreen(edited("PreToolUse", 2, 1), 0, 0, 8))}, "shows 1 of 2 PreToolUse hooks active", 1},
		{"disabled handler in the overlap", func(*fakeOrca) {}, []string{historyLost(t, trustedLast(edited("SessionStart", 2, 1), 8)), historyLost(t, trustedScreen(edited("SessionStart", 2, 1), 0, 0, 8))}, "shows 1 of 2 SessionStart hooks active", 1},
		{"installed count changes", func(*fakeOrca) {}, []string{historyLost(t, trustedLast(edited("Stop", 3, 3), 8)), reads[4]}, "shows 3 of 3 Stop hooks active", 1},
		{"trusted edges disagree", func(*fakeOrca) {}, []string{historyLost(t, trustedLast(edited("UserPromptSubmit", 2, 1), 8)), reads[4]}, "changed the UserPromptSubmit counts", 1},
		{"trusted edges reorder rows", func(*fakeOrca) {}, []string{historyLost(t, trustedLast(swapped, 8)), reads[4]}, "out of the supported order", 1},
		{"trusted view lists an unknown event", func(*fakeOrca) {}, []string{historyLost(t, trustedLast(slices.Concat(active[:4], unknown[11:], active[5:]), 8)), reads[4]}, "unsupported event row", 1},
		{"trusted view at neither edge after End", func(*fakeOrca) {}, []string{historyLost(t, trustedScreen(active, 4, 10, 8)), reads[4]}, "selects Stop, at neither edge of its events", 0},
		{"trusted edges list an unknown event", func(*fakeOrca) {}, []string{reads[3], historyLost(t, trustedScreen(slices.Concat(unknown[11:], active[1:]), 0, 0, 8))}, "moved from Interrupt to Notification, not its other edge", 1},
		{"trusted first edge is limited", func(*fakeOrca) {}, []string{reads[3], frameOf(t, "screen", false, true, trustedScreen(active, 0, 0, 8))}, "limited screen", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fake := codexFake(t)
				tt.edit(fake)
				for _, screen := range slices.Concat(reads[:3], tt.after) {
					fake.on(readScreen, screen)
				}
				probe := &probeRecorder{t: t, fake: fake, out: exactProbe}
				_, err := runCodex(t, fake, probe.review(captainPins), refusalPoll)
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("Bootstrap = %v, want %q", err, tt.want)
				}
				assertSent(t, fake, map[string]int{sendEnd: 1, sendHome: tt.homes, sendTrust: 1, sendEscape: 0, waitIdle: 0, sendDown: 0, sendUp: 0})
			})
		})
	}
}

func TestCodexReviewRefusesRepliesForAnotherTerminal(t *testing.T) {
	reads := acceptedReads(t)
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
		{"first browser view", read(1), otherRead, 1, map[string]int{sendEnter: 1, sendEnd: 0}},
		{"last edge view", read(2), otherRead, 1, map[string]int{sendEnd: 1, sendTrust: 0}},
		{"trusted browser", read(3), otherRead, 1, map[string]int{sendTrust: 1, sendHome: 0, sendEscape: 0, waitIdle: 0}},
		{"trusted opposite edge", read(4), otherRead, 1, map[string]int{sendTrust: 1, sendHome: 1, sendEscape: 0, waitIdle: 0}},
		{"Enter receipt", answer(sendEnter, otherTerminal(ok(accepted))), otherSend, 1, map[string]int{sendEnter: 1, sendEnd: 0}},
		{"End receipt", answer(sendEnd, otherTerminal(ok(accepted))), otherSend, 1, map[string]int{sendEnd: 1, sendTrust: 0}},
		{"trust receipt", answer(sendTrust, otherTerminal(ok(accepted))), otherSend, 1, map[string]int{sendTrust: 1, sendHome: 0, sendEscape: 0}},
		{"Home receipt", answer(sendHome, otherTerminal(ok(accepted))), otherSend, 1, map[string]int{sendTrust: 1, sendHome: 1, sendEscape: 0}},
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
	reads := acceptedReads(t)
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
			assertSent(t, fake, map[string]int{sendEnd: 1, sendHome: 1, sendTrust: 1, sendEscape: 1, waitIdle: 1})
		})
	}
	frames := []struct {
		name   string
		at     int
		probes int
		sent   map[string]int
	}{
		{"prompt", 0, 0, map[string]int{sendEnter: 0}},
		{"first browser", 1, 1, map[string]int{sendEnter: 1, sendEnd: 0}},
		{"last edge", 2, 1, map[string]int{sendEnd: 1, sendTrust: 0}},
		{"trusted browser", 3, 1, map[string]int{sendTrust: 1, sendHome: 0, sendEscape: 0, waitIdle: 0}},
		{"trusted opposite edge", 4, 1, map[string]int{sendTrust: 1, sendHome: 1, sendEscape: 0, waitIdle: 0}},
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
	reads := acceptedReads(t)
	short := orca.Poll{Interval: time.Millisecond, Timeout: 500 * time.Millisecond}
	tests := []struct {
		name    string
		command string
		sent    map[string]int
		stalled string
		moves   int
	}{
		{"probe", "", map[string]int{sendEnter: 0}, "hooks.probe", 0},
		{"Enter", sendEnter, map[string]int{sendEnter: 1, sendEnd: 0}, "remote.enter", 0},
		{"End", sendEnd, map[string]int{sendEnd: 1, sendTrust: 0}, "remote.key", 0},
		{"trust", sendTrust, map[string]int{sendTrust: 1, sendHome: 0}, "remote.key", 1},
		{"Home", sendHome, map[string]int{sendHome: 1, sendEscape: 0}, "remote.key", 1},
		{"Escape", sendEscape, map[string]int{sendEscape: 1, waitIdle: 0}, "remote.key", 2},
		{"idle wait", waitIdle, map[string]int{sendTrust: 1, sendEscape: 1, waitIdle: 1}, "remote.waitIdle", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				logs := recordTimings(t)
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
				got := timings(t, logs, "deadline", "term-1")
				stalled, reviewed := tt.stalled+" ok=false seconds=0.5", fmt.Sprintf("hooks.review moves=%d ok=false seconds=0.5", tt.moves)
				if !slices.Contains(got, stalled) || len(got) < 2 || !slices.Equal(got[len(got)-2:], []string{reviewed, "bootstrap ok=false seconds=0.5 steps=0"}) {
					t.Errorf("timings = %q, want %q, %q and a failed bootstrap", got, stalled, reviewed)
				}
			})
		})
	}
}

func TestParseHookProbeReturnsOnlySafeFields(t *testing.T) {
	probe, err := orca.ParseHookProbe([]byte(exactProbe+"\n"), captainPins[0], orca.ServerElectron)
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
			_, err := orca.ParseHookProbe([]byte(tt.out), captainPins[0], orca.ServerElectron)
			if err == nil || !strings.Contains(err.Error(), tt.want) || strings.Contains(err.Error(), leaked) {
				t.Errorf("ParseHookProbe = %v, want %q without the probe's values", err, tt.want)
			}
		})
	}
	var refusal orca.HookRefusal
	_, err = orca.ParseHookProbe([]byte(`{"schema":1,"outcome":"refused","reason":"missing","event":"PreToolUse","record":1}`), captainPins[0], orca.ServerElectron)
	if !errors.As(err, &refusal) || refusal != (orca.HookRefusal{Reason: "missing", Event: "PreToolUse", Record: 1}) {
		t.Errorf("refusal = %+v, %v", refusal, err)
	}
}
