package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yasyf/cc-remote/internal/config"
	"github.com/yasyf/cc-remote/internal/frontends/orca"
	"github.com/yasyf/cc-remote/internal/providers"
	"github.com/yasyf/cc-remote/internal/state"
)

func standInKeeper(t *testing.T) *atomic.Int32 {
	t.Helper()
	starts := &atomic.Int32{}
	original := keepCommand
	keepCommand = func(string) (*exec.Cmd, error) {
		starts.Add(1)
		return exec.Command("true"), nil
	}
	t.Cleanup(func() { keepCommand = original })
	return starts
}

func stubForwards(t *testing.T, hung, refused []string) func() []string {
	t.Helper()
	bin, dir := t.TempDir(), t.TempDir()
	opens, up := filepath.Join(dir, "opens"), filepath.Join(dir, "up")
	script := "#!/bin/sh\nfor host do :; done\ncase \" $* \" in\n" +
		"*' -O check '*) grep -qx \"$host\" " + up + " 2>/dev/null; exit $?;;\n" +
		"*' -O exit '*) exit 0;;\n" +
		"*' -N -L '*)\n" +
		"  case ' " + strings.Join(hung, " ") + " ' in *\" $host \"*) exec sleep 60;; esac\n" +
		"  case ' " + strings.Join(refused, " ") + " ' in *\" $host \"*) exit 255;; esac\n" +
		"  echo \"$host\" >> " + opens + "; echo \"$host\" >> " + up + ";;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	return func() []string {
		raw, err := os.ReadFile(opens)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		return strings.Fields(string(raw))
	}
}

func (w *firstWorker) liveTask(name string) *orcaTask {
	w.t.Helper()
	control, err := state.NewOrcaControl()
	if err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(control)) })
	dir := w.session.Config.State()
	task := &orcaTask{SchemaVersion: orcaTaskSchema, Workspace: name, Provider: "fake", Profile: "lean", Machine: name, Environment: name, Port: 7001, Forward: &orcaTunnel{Host: name, Config: "/recorded/config", Control: control, Log: dir.OrcaForwardLog(name)}}
	if err := state.Save(dir.Orca(name), task); err != nil {
		w.t.Fatal(err)
	}
	return task
}

func storageReachable(context.Context, *orcaTask) error { return nil }

func storageGoneFor(machines ...string) storageCheck {
	return func(_ context.Context, task *orcaTask) error {
		if slices.Contains(machines, task.Machine) {
			return fmt.Errorf("sprite %s: %w", task.Machine, providers.ErrNoStorage)
		}
		return nil
	}
}

func TestKeepReopensAKeptForwardThatExitedButNotAStoppedOrUnkeptOne(t *testing.T) {
	w := newWarmPool(t)
	opened := stubForwards(t, nil, nil)
	starts := standInKeeper(t)
	dir := w.session.Config.State()
	quiet := slog.New(slog.DiscardHandler)
	driver := orcaDriver{state: dir, log: quiet}
	for _, name := range []string{"lane-a", "lane-b", "lane-c"} {
		w.laneWorkspace(name)
		task := w.liveTask(name)
		if name == "lane-c" {
			continue
		}
		if err := driver.keep(task, w.session.Config.Path); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := stopLane(t.Context(), w.session, newSavedEnvironments(t).client(), "lane-b"); err != nil {
		t.Fatalf("stop lane-b = %v", err)
	}

	gone := map[string]time.Time{}
	kept, err := reopenForwards(t.Context(), dir, quiet, storageReachable, gone)
	if err != nil || kept != 1 {
		t.Fatalf("a keeper pass = %d kept, %v; want lane-a alone kept", kept, err)
	}
	if got := opened(); !slices.Equal(got, []string{"lane-a"}) {
		t.Errorf("forwards opened = %q, want only lane-a's, not the stopped lane-b's or the never-kept lane-c's", got)
	}
	if _, err := reopenForwards(t.Context(), dir, quiet, storageReachable, gone); err != nil || len(opened()) != 1 {
		t.Errorf("a pass over a forward that is up = %v, opened %q; want nothing reopened", err, opened())
	}
	if starts.Load() != 2 {
		t.Errorf("keeper starts = %d, want one per kept task", starts.Load())
	}
}

func TestKeepRunsOncePerStateDirectoryAndExitsWhenNothingIsKept(t *testing.T) {
	w := newWarmPool(t)
	opened := stubForwards(t, nil, nil)
	standInKeeper(t)
	dir := w.session.Config.State()
	quiet := slog.New(slog.DiscardHandler)
	w.laneWorkspace("lane-a")
	if err := (orcaDriver{state: dir, log: quiet}).keep(w.liveTask("lane-a"), w.session.Config.Path); err != nil {
		t.Fatal(err)
	}

	unlock, held, err := state.TryLock(dir.OrcaKeeper())
	if err != nil || !held {
		t.Fatalf("hold the keeper lock = %t, %v", held, err)
	}
	if err := keepForwards(t.Context(), dir, quiet, noPrune, storageReachable, nil, successor{}); err != nil || len(opened()) != 0 {
		t.Errorf("a keep beside a running one = %v, opened %q; want it to exit at once", err, opened())
	}
	unlock()

	if err := state.Remove(dir.OrcaKept("lane-a")); err != nil {
		t.Fatal(err)
	}
	if err := keepForwards(t.Context(), dir, quiet, noPrune, storageReachable, nil, successor{}); err != nil || len(opened()) != 0 {
		t.Errorf("a keep with nothing kept = %v, opened %q; want it to exit at once", err, opened())
	}
}

func (w *firstWorker) keptLanes(names ...string) {
	w.t.Helper()
	driver := orcaDriver{state: w.session.Config.State(), log: slog.New(slog.DiscardHandler)}
	for _, name := range names {
		w.laneWorkspace(name)
		if err := driver.keep(w.liveTask(name), w.session.Config.Path); err != nil {
			w.t.Fatal(err)
		}
	}
}

func TestKeepGivesEachReopenItsOwnDeadlineSoAHungForwardHoldsUpNoOther(t *testing.T) {
	within := reopenWithin
	reopenWithin = 200 * time.Millisecond
	t.Cleanup(func() { reopenWithin = within })
	w := newWarmPool(t)
	opened := stubForwards(t, []string{"lane-hung"}, nil)
	standInKeeper(t)
	w.keptLanes("lane-hung", "lane-next")
	var logged bytes.Buffer

	started := time.Now()
	kept, err := reopenForwards(t.Context(), w.session.Config.State(), slog.New(slog.NewTextHandler(&logged, nil)), storageReachable, map[string]time.Time{})
	if err != nil || kept != 2 {
		t.Fatalf("a pass = %d kept, %v; want both still kept", kept, err)
	}
	if took := time.Since(started); took > 10*time.Second {
		t.Errorf("the pass took %s; want the hung reopen cut at %s", took, reopenWithin)
	}
	if got := opened(); !slices.Equal(got, []string{"lane-next"}) {
		t.Errorf("forwards opened = %q, want lane-next's, behind the hung lane-hung", got)
	}
	if !strings.Contains(logged.String(), "workspace=lane-hung") || !strings.Contains(logged.String(), "did not finish within 200ms") {
		t.Errorf("the log %q does not name the hung reopen and its deadline", logged.String())
	}
}

func TestKeepReleasesAForwardWhoseMachineKeepsReportingItsStorageGoneAndSaysSoOnce(t *testing.T) {
	gone := storageGone
	storageGone = 0
	t.Cleanup(func() { storageGone = gone })
	w := newWarmPool(t)
	opened := stubForwards(t, nil, []string{"lane-dead", "lane-down"})
	standInKeeper(t)
	w.keptLanes("lane-dead", "lane-down", "lane-live")
	dir := w.session.Config.State()
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, nil))
	seen := map[string]time.Time{}
	storage := storageGoneFor("lane-dead")

	if kept, err := reopenForwards(t.Context(), dir, log, storage, seen); err != nil || kept != 3 {
		t.Fatalf("the first pass = %d kept, %v; want all three kept, since one look at a gone storage releases nothing", kept, err)
	}
	if strings.Contains(logged.String(), "lane-dead") {
		t.Errorf("the first look at lane-dead's gone storage logged %q, want nothing until it is released", logged.String())
	}
	if kept, err := reopenForwards(t.Context(), dir, log, storage, seen); err != nil || kept != 2 {
		t.Fatalf("the second pass = %d kept, %v; want lane-dead released", kept, err)
	}
	if _, err := os.Stat(dir.OrcaKept("lane-dead")); !os.IsNotExist(err) {
		t.Errorf("lane-dead is still kept: %v", err)
	}
	for _, still := range []string{"lane-down", "lane-live"} {
		if _, err := os.Stat(dir.OrcaKept(still)); err != nil {
			t.Errorf("%s was released, want it kept: %v", still, err)
		}
	}
	if kept, err := reopenForwards(t.Context(), dir, log, storage, seen); err != nil || kept != 2 {
		t.Fatalf("the third pass = %d kept, %v", kept, err)
	}
	if n := strings.Count(logged.String(), "released a kept forward"); n != 1 || !strings.Contains(logged.String(), "workspace=lane-dead") {
		t.Errorf("logged %d releases in %q, want lane-dead's once", n, logged.String())
	}
	if n := strings.Count(logged.String(), "did not reopen, and the next pass tries again\" workspace=lane-down"); n != 3 {
		t.Errorf("lane-down, whose storage answers, logged %d failed reopens over three passes, want 3: %q", n, logged.String())
	}
	if got := opened(); !slices.Equal(got, []string{"lane-live"}) {
		t.Errorf("forwards opened = %q, want lane-live's alone", got)
	}
}

func TestKeepForgetsAGoneStorageSightingOnceItsLaneIsNoLongerKept(t *testing.T) {
	gone := storageGone
	storageGone = 0
	t.Cleanup(func() { storageGone = gone })
	w := newWarmPool(t)
	stubForwards(t, nil, []string{"lane-dead", "lane-other"})
	standInKeeper(t)
	w.keptLanes("lane-dead", "lane-other")
	dir := w.session.Config.State()
	quiet := slog.New(slog.DiscardHandler)
	seen := map[string]time.Time{}
	storage := storageGoneFor("lane-dead")
	if _, err := reopenForwards(t.Context(), dir, quiet, storage, seen); err != nil {
		t.Fatal(err)
	}
	if err := state.Remove(dir.OrcaKept("lane-dead")); err != nil {
		t.Fatal(err)
	}
	if _, err := reopenForwards(t.Context(), dir, quiet, storage, seen); err != nil {
		t.Fatal(err)
	}
	if err := state.Write(dir.OrcaKept("lane-dead"), nil); err != nil {
		t.Fatal(err)
	}
	if kept, err := reopenForwards(t.Context(), dir, quiet, storage, seen); err != nil || kept != 2 {
		t.Fatalf("the first pass after lane-dead was kept again = %d kept, %v; want its sighting from before counted as new", kept, err)
	}
	if kept, err := reopenForwards(t.Context(), dir, quiet, storage, seen); err != nil || kept != 1 {
		t.Errorf("the second pass after it was kept again = %d kept, %v; want it released", kept, err)
	}
}

func TestKeepSendsEachKeptWorkerEveryGitHubAppTokenTheMacMintsOnceItsForwardIsUp(t *testing.T) {
	w := newWarmPool(t)
	standInKeeper(t)
	dir := w.session.Config.State()
	quiet := slog.New(slog.DiscardHandler)
	driver := orcaDriver{state: dir, log: quiet}
	for _, name := range []string{"lane-a", "lane-b"} {
		w.laneWorkspace(name)
		if err := driver.keep(w.liveTask(name), w.session.Config.Path); err != nil {
			t.Fatal(err)
		}
	}
	bin, sent, down := t.TempDir(), t.TempDir(), t.TempDir()
	ssh := "#!/bin/sh\nfor arg do\n  test \"$arg\" = sh && break\n  host=$arg\ndone\ncase \" $* \" in *' -O check '*) test ! -e " + down + "/\"$host\"; exit $?;; esac\ncat >> " + sent + "/\"$host\"\n"
	minted := filepath.Join(bin, "minted")
	mint := filepath.Join(bin, "mint")
	for path, body := range map[string]string{filepath.Join(bin, "ssh"): ssh, mint: "#!/bin/sh\ncat " + minted + "\n"} {
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	received := func(name string) string {
		raw, err := os.ReadFile(filepath.Join(sent, name))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return string(raw)
	}
	tokens := &renewal{key: config.KeyGitHubApp, command: []string{mint}, capture: captureAppToken, script: orca.GitHubAppScript, sent: map[string][]byte{}}
	pass := func(line string) {
		t.Helper()
		if err := os.WriteFile(minted, []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := tokens.renew(t.Context(), dir, quiet); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(down, "lane-b"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	pass("{\"token\":\"first\"}\n")
	if a, b := received("lane-a"), received("lane-b"); a != `{"token":"first"}` || b != "" {
		t.Fatalf("after the first mint lane-a got %q and lane-b, whose forward is down, got %q", a, b)
	}
	if err := os.Remove(filepath.Join(down, "lane-b")); err != nil {
		t.Fatal(err)
	}
	pass("{\"token\":\"first\"}\n")
	if a, b := received("lane-a"), received("lane-b"); a != `{"token":"first"}` || b != `{"token":"first"}` {
		t.Errorf("once lane-b's forward is up, lane-a got %q and lane-b %q; want the same token once each", a, b)
	}
	pass("{\"token\":\"second\"}\n")
	if a := received("lane-a"); a != `{"token":"first"}{"token":"second"}` {
		t.Errorf("after the second mint lane-a got %q, want both tokens in turn", a)
	}
	if err := os.WriteFile(mint, []byte("#!/bin/sh\nexit 3\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := tokens.renew(t.Context(), dir, quiet); err != nil || received("lane-b") != `{"token":"first"}{"token":"second"}` {
		t.Errorf("a failed mint = %v, lane-b got %q; want no error and nothing new sent", err, received("lane-b"))
	}
}

func TestKeepSendsEachKeptWorkerANewAWSSessionOnceTheOneItHoldsHasUnderAnHourLeft(t *testing.T) {
	w := newWarmPool(t)
	standInKeeper(t)
	dir := w.session.Config.State()
	quiet := slog.New(slog.DiscardHandler)
	w.keptLanes("lane-a", "lane-b")
	bin, sent := t.TempDir(), t.TempDir()
	ssh := "#!/bin/sh\nfor arg do\n  test \"$arg\" = sh && break\n  host=$arg\ndone\ncase \" $* \" in *' -O check '*) exit 0;; esac\ncat >> " + sent + "/\"$host\"\necho >> " + sent + "/\"$host\"\n"
	minted, mints := filepath.Join(bin, "minted"), filepath.Join(bin, "mints")
	mint := filepath.Join(bin, "mint")
	for path, body := range map[string]string{filepath.Join(bin, "ssh"): ssh, mint: "#!/bin/sh\necho >> " + mints + "\ncat " + minted + "\n"} {
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	session := func(id string, left time.Duration) string {
		return fmt.Sprintf("{\n  \"Version\": 1,\n  \"AccessKeyId\": %q,\n  \"SecretAccessKey\": \"secret\",\n  \"SessionToken\": \"token\",\n  \"Expiration\": %q\n}\n", id, time.Now().Add(left).UTC().Format(time.RFC3339))
	}
	line := func(out string) string {
		var compact bytes.Buffer
		if err := json.Compact(&compact, []byte(out)); err != nil {
			t.Fatal(err)
		}
		return compact.String() + "\n"
	}
	sessions := &renewal{key: config.KeyAWS, command: []string{mint}, capture: captureAWS, script: orca.AWSScript, sent: map[string][]byte{}}
	pass := func(out string) {
		t.Helper()
		if err := os.WriteFile(minted, []byte(out), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := sessions.renew(t.Context(), dir, quiet); err != nil {
			t.Fatal(err)
		}
	}
	received := func(name string) string {
		raw, err := os.ReadFile(filepath.Join(sent, name))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return string(raw)
	}
	minting := func() int {
		raw, err := os.ReadFile(mints)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return strings.Count(string(raw), "\n")
	}

	expiring, fresh, unused := session("ASIA-expiring", 50*time.Minute), session("ASIA-fresh", 4*time.Hour), session("ASIA-unused", 4*time.Hour)
	pass(expiring)
	pass(fresh)
	pass(unused)
	want := line(expiring) + line(fresh)
	if a, b := received("lane-a"), received("lane-b"); a != want || b != want {
		t.Errorf("lane-a got %q and lane-b %q; want the session with 50 minutes left, then the fresh one, each once", a, b)
	}
	if n := minting(); n != 2 {
		t.Errorf("the mint ran %d times over three passes, want twice: once for the first session and once because it had under an hour left, not while the fresh one has four hours", n)
	}
}

func TestAKeepOfANewerReleaseTakesOverFromTheRunningOne(t *testing.T) {
	w := newWarmPool(t)
	stubForwards(t, nil, nil)
	standInKeeper(t)
	dir := w.session.Config.State()
	quiet := slog.New(slog.DiscardHandler)
	w.keptLanes("lane-a")
	var started []successor
	original := execKeeper
	execKeeper = func(next successor) error {
		started = append(started, next)
		return errors.New("exec refused")
	}
	t.Cleanup(func() { execKeeper = original })
	running := successor{Version: "0.27.9", Binary: "/running/cc-remote", Config: "/running/config.yaml"}
	newer := successor{Version: "0.27.10", Binary: "/newer/cc-remote", Config: "/newer/config.yaml"}
	older := successor{Version: "0.27.8", Binary: "/older/cc-remote", Config: "/older/config.yaml"}

	unlock, held, err := state.TryLock(dir.OrcaKeeper())
	if err != nil || !held {
		t.Fatalf("hold the keeper lock = %t, %v", held, err)
	}
	for _, offered := range []successor{newer, older, {Version: "dev", Binary: "/dev/cc-remote"}} {
		if err := keepForwards(t.Context(), dir, quiet, noPrune, storageReachable, nil, offered); err != nil {
			t.Fatalf("a %s keep beside the running one = %v; want it to exit at once", offered.Version, err)
		}
	}
	unlock()
	if next, err := readSuccessor(dir); err != nil || next == nil || *next != newer {
		t.Fatalf("recorded successor = %+v, %v; want the 0.27.10 keep, not the older or dev one offered after it", next, err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := keepForwards(ctx, dir, quiet, noPrune, storageReachable, nil, running); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the 0.27.9 keep = %v; want it to keep holding after the handoff failed", err)
	}
	if !slices.Equal(started, []successor{newer}) {
		t.Errorf("keeps started = %+v, want the 0.27.10 one once", started)
	}
	if _, err := os.Stat(dir.OrcaKeeperSuccessor()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the successor that did not start is still recorded: %v", err)
	}

	if err := state.Save(dir.OrcaKeeperSuccessor(), older); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := keepForwards(ctx, dir, quiet, noPrune, storageReachable, nil, running); !errors.Is(err, context.DeadlineExceeded) || len(started) != 1 {
		t.Errorf("the 0.27.9 keep beside a recorded 0.27.8 successor = %v, started %+v; want no handoff to an older release", err, started)
	}
}
