package orca

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	KeyEscape       = "\x1b"
	CaptainPlugin   = "captain-hook"
	keyTrust        = "t"
	keyHome         = "\x1b[H"
	keyEnd          = "\x1b[F"
	screenSource    = "screen"
	probeSchema     = 1
	reviewBlocked   = `"agent-hooks-review-prompt"`
	trustBlocked    = `"agent-trust-workspace"`
	browserSelector = "›"
	hooksTitle      = "Hooks"
	hooksSubtitle   = "Lifecycle hooks from config and enabled plugins."
	reviewFooter    = "t trust all · enter review · esc close"
	trustedFooter   = "enter details · esc close"
	moreAbove       = "↑"
	moreBelow       = "↓"
	reviewChoice    = "1. Review hooks"
)

const HookProbeScript = `import hashlib
import json
import os
import sys
import tomllib

CAPTAIN = ("SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop")
NATIVE = ("SessionStart", "UserPromptSubmit", "PreToolUse", "PermissionRequest", "PostToolUse", "SubagentStart", "SubagentStop", "Stop")
BOOKKEEPING = {"state", "enabled"}


class Refusal(Exception):
    def __init__(self, reason, event="", record=-1):
        super().__init__(reason)
        self.reason = reason
        self.event = event
        self.record = record


def quote(value):
    return "'" + value.replace("'", "'\\''") + "'"


def unique(pairs):
    keys = [key for key, _ in pairs]
    if len(keys) != len(set(keys)):
        raise Refusal("duplicate-key")
    return dict(pairs)


def read(path, reason):
    try:
        with open(path, "rb") as source:
            return source.read()
    except OSError:
        raise Refusal(reason)


def parse(raw, reason):
    try:
        return json.loads(raw, object_pairs_hook=unique)
    except ValueError:
        raise Refusal(reason)


def check_config(table):
    for key, value in table.items():
        if key == "hooks":
            if not isinstance(value, dict) or set(value) - BOOKKEEPING:
                raise Refusal("inline-hooks")
        elif isinstance(value, dict):
            check_config(value)


def config(path):
    try:
        with open(path, "rb") as source:
            table = tomllib.load(source)
    except FileNotFoundError:
        return
    except (OSError, tomllib.TOMLDecodeError):
        raise Refusal("config")
    check_config(table)


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False)


def probe(plugin, version):
    home = os.path.expanduser("~")
    if not home.startswith("/") or os.path.normpath(home) != home:
        raise Refusal("home")
    name, _, market = plugin.partition("@")
    root = "/".join((home, ".claude/plugins/cache", market, name, version))
    installed = parse(read(home + "/.claude/plugins/installed_plugins.json", "metadata"), "metadata")
    plugins = installed.get("plugins") if isinstance(installed, dict) else None
    entries = plugins.get(plugin) if isinstance(plugins, dict) else None
    users = [entry for entry in entries if isinstance(entry, dict) and entry.get("scope") == "user"] if isinstance(entries, list) else []
    if len(users) != 1 or users[0].get("version") != version or users[0].get("installPath") != root:
        raise Refusal("metadata")
    manifest = parse(read(root + "/.claude-plugin/plugin.json", "manifest"), "manifest")
    if not isinstance(manifest, dict) or manifest.get("name") != name or manifest.get("version") != version:
        raise Refusal("manifest")
    hook = root + "/bin/hook"
    if not os.path.isfile(hook) or not os.access(hook, os.X_OK):
        raise Refusal("executable")
    config(home + "/.codex/config.toml")
    raw = read(home + "/.codex/hooks.json", "hooks")
    document = parse(raw, "hooks")
    if not isinstance(document, dict) or set(document) != {"hooks"} or not isinstance(document["hooks"], dict):
        raise Refusal("hooks")
    table = document["hooks"]
    if set(table) - set(NATIVE):
        raise Refusal("unexpected-event")
    script = quote(home + "/.orca/agent-hooks/codex-hook.sh")
    native = {"hooks": [{"type": "command", "command": "if [ -f " + script + " ] && [ -r " + script + " ] && [ -x " + script + " ]; then /bin/sh " + script + "; else { command -p cat 2>/dev/null || cat; } >/dev/null 2>&1 || :; fi", "timeout": 10}]}
    events = {}
    for event in NATIVE:
        want = [native]
        if event in CAPTAIN:
            want.append({"hooks": [{"type": "command", "command": "CAPT_HOOK_PROVIDER=codex " + quote(hook) + " run " + event}]})
        groups = table.get(event)
        if not isinstance(groups, list):
            raise Refusal("missing", event, 0)
        expected = [canonical(group) for group in want]
        remaining = list(expected)
        for index, group in enumerate(groups):
            text = canonical(group)
            if text not in remaining:
                raise Refusal("mismatch", event, index)
            remaining.remove(text)
        if remaining:
            raise Refusal("missing", event, expected.index(remaining[0]))
        events[event] = len(groups)
    return {"schema": 1, "outcome": "exact", "reason": "", "event": "", "record": -1, "home": home, "root": root, "hooksSha256": hashlib.sha256(raw).hexdigest(), "definitions": sum(events.values()), "events": events}


def main():
    try:
        result = probe(sys.argv[1], sys.argv[2])
    except Refusal as refusal:
        result = {"schema": 1, "outcome": "refused", "reason": refusal.reason, "event": refusal.event, "record": refusal.record}
    except Exception:
        result = {"schema": 1, "outcome": "refused", "reason": "probe-error", "event": "", "record": -1}
    print(json.dumps(result, sort_keys=True))


main()
`

var (
	nativeHookEvents  = []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PermissionRequest", "PostToolUse", "SubagentStart", "SubagentStop", "Stop"}
	captainHookEvents = []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop"}
	browserEvents     = []string{"PreToolUse", "PermissionRequest", "PostToolUse", "PreCompact", "PostCompact", "SessionStart", "SessionEnd", "UserPromptSubmit", "SubagentStart", "SubagentStop", "Stop", "Interrupt"}
	probeRefusals     = []string{"home", "metadata", "manifest", "executable", "config", "inline-hooks", "hooks", "duplicate-key", "unexpected-event", "mismatch", "missing", "probe-error"}
	promptLines       = []string{"Hooks need review", "", "Hooks can run outside the sandbox after you trust them.", reviewChoice, "2. Trust all and continue", "3. Continue without trusting (hooks won't run)", "enter confirm · esc skip"}
	promptCount       = regexp.MustCompile(`^(\d+) hooks? (?:are|is) new or changed\.$`)
	pendingLine       = regexp.MustCompile(`^\s*⚠ (\d+) hooks? need review before they can run\.$`)
	reviewHeader      = regexp.MustCompile(`^\s*Event\s+Installed\s+Active\s+Review\s+Description$`)
	trustedHeader     = regexp.MustCompile(`^\s*Event\s+Installed\s+Active\s+Description$`)
	reviewRow         = regexp.MustCompile(`^(›| ) ([A-Za-z]+) +(\d+) +(\d+) +(\d+) +\S.*$`)
	trustedRow        = regexp.MustCompile(`^(›| ) ([A-Za-z]+) +(\d+) +(\d+) +\S.*$`)
	sha256Hex         = regexp.MustCompile(`^[0-9a-f]{64}$`)
	folderTrust       = Gate{Name: "trust", Screen: regexp.MustCompile(`(?ms)^ *Folder access *$.*^ *Trust this folder\? Codex can read.*^ *enter continue · esc quit *$`), Choice: regexp.MustCompile(`^1\. Trust and continue$`), Move: KeyUp, Trust: true}
)

type Pin struct {
	ID      string
	Version string
}

type HookReview struct {
	Captain []Pin
	Exec    func(ctx context.Context, argv []string) ([]byte, error)
}

type HookProbe struct {
	Schema      int            `json:"schema"`
	Outcome     string         `json:"outcome"`
	Reason      string         `json:"reason"`
	Event       string         `json:"event"`
	Record      int            `json:"record"`
	Home        string         `json:"home"`
	Root        string         `json:"root"`
	HooksSHA256 string         `json:"hooksSha256"`
	Definitions int            `json:"definitions"`
	Events      map[string]int `json:"events"`
}

type HookRefusal struct {
	Reason string
	Event  string
	Record int
}

type hookRow struct {
	Event     string
	Installed int
	Active    int
	Review    int
}

type hookBrowser struct {
	pending  int
	rows     []hookRow
	selected string
	above    bool
	below    bool
}

type hookRows struct {
	order  []string
	counts map[string]hookRow
}

func CodexStartup(review HookReview) Startup {
	return Startup{Gates: []Gate{folderTrust}, Hooks: &review}
}

func (e HookRefusal) Error() string {
	if e.Event == "" {
		return "the worker's Codex hooks differ from the accepted set: " + e.Reason
	}
	return fmt.Sprintf("the worker's Codex hooks differ from the accepted set: %s at %s record %d", e.Reason, e.Event, e.Record)
}

func (h HookReview) captain() (Pin, error) {
	if len(h.Captain) != 1 {
		return Pin{}, fmt.Errorf("the accepted inventory selects %d Captain Hook plugins, not exactly one", len(h.Captain))
	}
	pin := h.Captain[0]
	name, market, ok := strings.Cut(pin.ID, "@")
	if !ok || name != CaptainPlugin || !agentValue.MatchString(market) || !agentValue.MatchString(pin.Version) {
		return Pin{}, errors.New("the accepted Captain Hook pin is not a plugin id and version")
	}
	return pin, nil
}

func (h HookReview) probe(ctx context.Context) (HookProbe, error) {
	pin, err := h.captain()
	if err != nil {
		return HookProbe{}, err
	}
	started := time.Now()
	out, err := h.Exec(ctx, []string{"python3", "-c", HookProbeScript, pin.ID, pin.Version})
	observe(ctx, "hooks.probe", started, err)
	if err != nil {
		return HookProbe{}, fmt.Errorf("probe the worker's Codex hooks: %w", err)
	}
	return ParseHookProbe(out, pin)
}

func ParseHookProbe(out []byte, pin Pin) (HookProbe, error) {
	var probe HookProbe
	decoder := json.NewDecoder(bytes.NewReader(out))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&probe); err != nil || decoder.More() {
		return HookProbe{}, errors.New("the Codex hook probe printed no single result document")
	}
	if probe.Schema != probeSchema {
		return HookProbe{}, errors.New("the Codex hook probe answered with another schema")
	}
	switch probe.Outcome {
	case "refused":
		if !slices.Contains(probeRefusals, probe.Reason) || (probe.Event != "" && !slices.Contains(nativeHookEvents, probe.Event)) {
			return HookProbe{}, errors.New("the Codex hook probe refused for an unrecognized reason")
		}
		return HookProbe{}, HookRefusal{Reason: probe.Reason, Event: probe.Event, Record: probe.Record}
	case "exact":
	default:
		return HookProbe{}, errors.New("the Codex hook probe reported an unrecognized outcome")
	}
	name, market, _ := strings.Cut(pin.ID, "@")
	switch {
	case !path.IsAbs(probe.Home) || path.Clean(probe.Home) != probe.Home:
		return HookProbe{}, errors.New("the Codex hook probe reported no absolute runtime home")
	case probe.Root != path.Join(probe.Home, ".claude/plugins/cache", market, name, pin.Version):
		return HookProbe{}, errors.New("the Codex hook probe matched another Captain Hook root")
	case !sha256Hex.MatchString(probe.HooksSHA256):
		return HookProbe{}, errors.New("the Codex hook probe reported no hooks digest")
	case probe.Definitions != len(nativeHookEvents)+len(captainHookEvents) || !maps.Equal(probe.Events, multiplicity()):
		return HookProbe{}, errors.New("the Codex hook probe counted another set of definitions")
	}
	return probe, nil
}

func multiplicity() map[string]int {
	counts := map[string]int{}
	for _, event := range nativeHookEvents {
		counts[event] = expectedRow(event).Installed
	}
	return counts
}

func expectedRow(event string) hookRow {
	row := hookRow{Event: event}
	if slices.Contains(nativeHookEvents, event) {
		row.Installed, row.Active = 1, 1
	}
	if slices.Contains(captainHookEvents, event) {
		row.Installed, row.Review = row.Installed+1, 1
	}
	return row
}

func completeFrame(screen Screen) error {
	switch {
	case screen.Source != screenSource:
		return fmt.Errorf("terminal %s returned a %q read, not a rendered screen", screen.Handle, screen.Source)
	case screen.Limited == nil:
		return fmt.Errorf("terminal %s returned a screen without its limited flag, so its rows cannot be verified", screen.Handle)
	case *screen.Limited:
		return fmt.Errorf("terminal %s returned a limited screen, so its rows cannot be verified", screen.Handle)
	}
	return nil
}

func reviewPrompt(tail []string) (int, bool, error) {
	var found, selected []string
	pending := -1
	for _, line := range tail {
		text := strings.TrimSpace(line)
		for _, marker := range []string{browserSelector, selector} {
			if option, ok := strings.CutPrefix(text, marker); ok {
				text = strings.TrimSpace(option)
				selected = append(selected, text)
			}
		}
		if match := promptCount.FindStringSubmatch(text); match != nil {
			count, err := strconv.Atoi(match[1])
			if err != nil {
				return 0, false, errors.New("the hook review prompt has an unreadable count")
			}
			pending = count
			found = append(found, "")
			continue
		}
		if slices.Contains(promptLines, text) && text != "" {
			found = append(found, text)
		}
	}
	if !slices.Contains(found, promptLines[0]) {
		return 0, false, nil
	}
	if !slices.Equal(found, promptLines) || pending < 0 {
		return 0, false, errors.New("the hook review prompt is not the recognized native prompt")
	}
	if len(selected) > 1 || (len(selected) == 1 && selected[0] != reviewChoice) {
		return 0, false, errors.New("the hook review prompt selects an action other than Review hooks")
	}
	return pending, true, nil
}

func parseBrowser(tail []string, trusted bool) (hookBrowser, error) {
	lines := make([]string, len(tail))
	for i, line := range tail {
		lines[i] = strings.TrimRight(line, " ")
	}
	start := -1
	for i := 0; i+1 < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == hooksTitle && strings.TrimSpace(lines[i+1]) == hooksSubtitle {
			if start >= 0 {
				return hookBrowser{}, errors.New("the screen shows more than one hooks browser")
			}
			start = i + 2
		}
	}
	end := len(lines) - 1
	for end >= 0 && strings.TrimSpace(lines[end]) == "" {
		end--
	}
	footer, header, row := reviewFooter, reviewHeader, reviewRow
	if trusted {
		footer, header, row = trustedFooter, trustedHeader, trustedRow
	}
	if start < 0 || end < start || strings.TrimSpace(lines[end]) != footer {
		return hookBrowser{}, errors.New("the screen is not the recognized hooks browser")
	}
	browser := hookBrowser{}
	i := start
	if !trusted {
		match := pendingLine.FindStringSubmatch(lines[i])
		if match == nil {
			return hookBrowser{}, errors.New("the hooks browser shows no review count")
		}
		count, err := strconv.Atoi(match[1])
		if err != nil {
			return hookBrowser{}, errors.New("the hooks browser has an unreadable review count")
		}
		browser.pending = count
		i++
	}
	if i >= end || !header.MatchString(lines[i]) {
		return hookBrowser{}, errors.New("the hooks browser shows no recognized column header")
	}
	i++
	stop := end
	if i < stop && strings.TrimSpace(lines[i]) == moreAbove {
		browser.above = true
		i++
	}
	if stop > i && strings.TrimSpace(lines[stop-1]) == moreBelow {
		browser.below = true
		stop--
	}
	for ; i < stop; i++ {
		match := row.FindStringSubmatch(lines[i])
		if match == nil {
			return hookBrowser{}, errors.New("the hooks browser has a row it cannot read")
		}
		counts := make([]int, len(match)-3)
		for j, field := range match[3:] {
			n, err := strconv.Atoi(field)
			if err != nil {
				return hookBrowser{}, errors.New("the hooks browser has a count it cannot read")
			}
			counts[j] = n
		}
		parsed := hookRow{Event: match[2], Installed: counts[0], Active: counts[1]}
		if !trusted {
			parsed.Review = counts[2]
		}
		if slices.ContainsFunc(browser.rows, func(r hookRow) bool { return r.Event == parsed.Event }) {
			return hookBrowser{}, errors.New("the hooks browser lists an event twice")
		}
		if match[1] == browserSelector {
			if browser.selected != "" {
				return hookBrowser{}, errors.New("the hooks browser selects more than one row")
			}
			browser.selected = parsed.Event
		}
		browser.rows = append(browser.rows, parsed)
	}
	if len(browser.rows) == 0 || browser.selected == "" {
		return hookBrowser{}, errors.New("the hooks browser shows no selected row")
	}
	return browser, nil
}

func (b hookBrowser) atFirst() bool {
	return !b.above && b.selected == browserEvents[0] && b.rows[0].Event == b.selected
}

func (b hookBrowser) atLast() bool {
	return !b.below && b.selected == browserEvents[len(browserEvents)-1] && b.rows[len(b.rows)-1].Event == b.selected
}

func (h *hookRows) add(view hookBrowser, pending int) error {
	if view.pending != pending {
		return fmt.Errorf("the hooks browser counts %d hooks to review, not %d", view.pending, pending)
	}
	at := -1
	for _, row := range view.rows {
		if !slices.Contains(browserEvents, row.Event) {
			return errors.New("the hooks browser lists an unsupported event row")
		}
		if prior, seen := h.counts[row.Event]; seen {
			index := slices.Index(h.order, row.Event)
			switch {
			case prior != row:
				return fmt.Errorf("the hooks browser changed the %s counts between views", row.Event)
			case at >= 0 && index != at+1:
				return errors.New("the hooks browser changed its row order between views")
			}
			at = index
			continue
		}
		if at != len(h.order)-1 {
			return errors.New("the hooks browser showed a new row out of order")
		}
		h.order = append(h.order, row.Event)
		h.counts[row.Event] = row
		at = len(h.order) - 1
	}
	return nil
}

func (h hookRows) check(probe HookProbe) error {
	var installed, active, review int
	for _, event := range browserEvents {
		want := expectedRow(event)
		row, seen := h.counts[event]
		switch {
		case !seen:
			return fmt.Errorf("the hooks browser lists no %s row", event)
		case row != want:
			return fmt.Errorf("the hooks browser shows %s with %d installed, %d active and %d to review, not %d, %d and %d", event, row.Installed, row.Active, row.Review, want.Installed, want.Active, want.Review)
		case row.Installed != probe.Events[event]:
			return fmt.Errorf("the hooks browser shows %d %s hooks, but the worker's hooks file defines %d", row.Installed, event, probe.Events[event])
		}
		installed, active, review = installed+row.Installed, active+row.Active, review+row.Review
	}
	if !slices.Equal(h.order, browserEvents) {
		return errors.New("the hooks browser lists its events out of the supported order")
	}
	if installed != probe.Definitions || active != len(nativeHookEvents) || review != len(captainHookEvents) {
		return fmt.Errorf("the hooks browser totals %d installed, %d active and %d to review", installed, active, review)
	}
	return nil
}

func (h hookRows) trusted(after hookRows) error {
	for _, event := range browserEvents {
		want := h.counts[event]
		row, seen := after.counts[event]
		switch {
		case !seen:
			return fmt.Errorf("the trusted hooks browser lists no %s row", event)
		case row.Installed != want.Installed || row.Active != want.Installed:
			return fmt.Errorf("the trusted hooks browser shows %d of %d %s hooks active", row.Active, row.Installed, event)
		}
	}
	if !slices.Equal(after.order, browserEvents) {
		return errors.New("the trusted hooks browser lists its events out of the supported order")
	}
	return nil
}

func (r Remote) browser(ctx context.Context, handle string, p Poll, trusted bool, ready func(hookBrowser) bool) (hookBrowser, error) {
	var view hookBrowser
	var last error
	_, err := poll(ctx, p, func(ctx context.Context) (struct{}, bool, error) {
		screen, err := r.Screen(ctx, handle)
		if err != nil {
			return struct{}{}, false, err
		}
		if err := completeFrame(screen); err != nil {
			return struct{}{}, false, err
		}
		parsed, err := parseBrowser(screen.Tail, trusted)
		if err != nil {
			last = err
			return struct{}{}, false, nil
		}
		view, last = parsed, nil
		return struct{}{}, ready(parsed), nil
	})
	if err != nil {
		return hookBrowser{}, errors.Join(err, last)
	}
	return view, nil
}

func (r Remote) opposite(ctx context.Context, handle string, p Poll, view hookBrowser, trusted bool) (hookBrowser, error) {
	key, edge := keyEnd, hookBrowser.atLast
	switch {
	case view.atLast():
		key, edge = keyHome, hookBrowser.atFirst
	case !view.atFirst():
		return hookBrowser{}, fmt.Errorf("the hooks browser selects %s, at neither edge of its events", view.selected)
	}
	if err := r.Key(ctx, handle, key); err != nil {
		return hookBrowser{}, err
	}
	next, err := r.browser(ctx, handle, p, trusted, func(b hookBrowser) bool { return b.selected != view.selected })
	if err != nil {
		return hookBrowser{}, fmt.Errorf("the hooks browser selection never moved from %s: %w", view.selected, err)
	}
	if !edge(next) {
		return hookBrowser{}, fmt.Errorf("the hooks browser selection moved from %s to %s, not its other edge", view.selected, next.selected)
	}
	return next, nil
}

func (r Remote) reviewHooks(ctx context.Context, handle string, pending int, review HookReview, p Poll) (err error) {
	started, moves := time.Now(), 0
	defer func() { observe(ctx, "hooks.review", started, err, "moves", moves) }()
	if pending != len(captainHookEvents) {
		return fmt.Errorf("the hook review prompt counts %d new hooks, not the %d accepted Captain Hook handlers", pending, len(captainHookEvents))
	}
	probe, err := review.probe(ctx)
	if err != nil {
		return err
	}
	if err := r.Enter(ctx, handle); err != nil {
		return err
	}
	view, err := r.browser(ctx, handle, p, false, func(hookBrowser) bool { return true })
	if err != nil {
		return fmt.Errorf("the Review hooks action did not open the hooks browser: %w", err)
	}
	if !view.atFirst() {
		return errors.New("the hooks browser did not open at its first row")
	}
	rows := hookRows{counts: map[string]hookRow{}}
	if err := rows.add(view, pending); err != nil {
		return err
	}
	if view, err = r.opposite(ctx, handle, p, view, false); err != nil {
		return err
	}
	moves++
	if err := rows.add(view, pending); err != nil {
		return err
	}
	if err := rows.check(probe); err != nil {
		return err
	}
	if err := r.Key(ctx, handle, keyTrust); err != nil {
		return err
	}
	if view, err = r.browser(ctx, handle, p, true, func(hookBrowser) bool { return true }); err != nil {
		return fmt.Errorf("the trust key did not produce the trusted hooks browser: %w", err)
	}
	other, err := r.opposite(ctx, handle, p, view, true)
	if err != nil {
		return err
	}
	moves++
	if view.atLast() {
		view, other = other, view
	}
	after := hookRows{counts: map[string]hookRow{}}
	for _, edge := range []hookBrowser{view, other} {
		if err := after.add(edge, 0); err != nil {
			return err
		}
	}
	if err := rows.trusted(after); err != nil {
		return err
	}
	if err := r.Key(ctx, handle, KeyEscape); err != nil {
		return err
	}
	_, err = r.WaitIdle(ctx, handle, idleTimeout)
	return err
}
