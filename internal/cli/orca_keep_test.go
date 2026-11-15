package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
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

func noOpen(task *orcaTask) (providers.Provider, error) {
	return nil, fmt.Errorf("this test opens no %s provider", task.Provider)
}

func storageGoneFor(machines ...string) storageCheck {
	return func(_ context.Context, task *orcaTask) error {
		if slices.Contains(machines, task.Machine) {
			return fmt.Errorf("sprite %s: %w", task.Machine, providers.ErrNoStorage)
		}
		return nil
	}
}

func tick(t *testing.T, k *keeper, task *keptTask) bool {
	t.Helper()
	kept, err := k.pass(t.Context(), task)
	if err != nil {
		t.Fatalf("a tick of %s = %v", task.name, err)
	}
	return kept
}

func keepPass(t *testing.T, k *keeper, tasks ...*keptTask) {
	t.Helper()
	names, err := keptNames(k.dir)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := keptKeys(k.dir, names, k.log)
	if err != nil {
		t.Fatal(err)
	}
	k.renew(t.Context(), keys)
	for _, task := range tasks {
		tick(t, k, task)
	}
}

func holding(t *testing.T, dir state.Dir, log *slog.Logger, renewals ...*renewal) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	held := make(chan error, 1)
	go func() {
		_, err := holdForwards(ctx, dir, log, noPrune, storageReachable, noOpen, renewals, "")
		held <- err
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-held; !errors.Is(err, context.Canceled) {
			t.Errorf("keep = %v, want it holding until canceled", err)
		}
	})
}

func eventually(t *testing.T, within time.Duration, what string, done func() bool) time.Duration {
	t.Helper()
	started := time.Now()
	for !done() {
		if time.Since(started) > within {
			t.Fatalf("%s did not happen within %s", what, within)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return time.Since(started)
}

func ticking(t *testing.T, every time.Duration) {
	t.Helper()
	original := keepEvery
	keepEvery = every
	t.Cleanup(func() { keepEvery = original })
}

func TestKeepReopensAKeptForwardThatExitedAndEndsTheTickOfAStoppedOne(t *testing.T) {
	w := newWarmPool(t)
	opened := stubForwards(t, nil, nil)
	starts := standInKeeper(t)
	dir := w.session.Config.State()
	quiet := slog.New(slog.DiscardHandler)
	w.keptLanes("lane-a", "lane-b")
	if _, err := stopLane(t.Context(), w.session, newSavedEnvironments(t).client(), "lane-b"); err != nil {
		t.Fatalf("stop lane-b = %v", err)
	}
	k := newKeeper(dir, quiet, storageReachable, noOpen, nil)
	a, b := newKeptTask("lane-a"), newKeptTask("lane-b")

	if !tick(t, k, a) {
		t.Fatal("a tick of lane-a ended it; want it kept")
	}
	if tick(t, k, b) {
		t.Error("a tick of the stopped lane-b kept it; want its ticks to end")
	}
	if got := opened(); !slices.Equal(got, []string{"lane-a"}) {
		t.Errorf("forwards opened = %q, want only lane-a's, not the stopped lane-b's", got)
	}
	if !tick(t, k, a) || len(opened()) != 1 {
		t.Errorf("a tick over a forward that is up opened %q; want nothing reopened", opened())
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
	if err := keepForwards(t.Context(), dir, quiet, noPrune, storageReachable, noOpen, nil, successor{}); err != nil || len(opened()) != 0 {
		t.Errorf("a keep beside a running one = %v, opened %q; want it to exit at once", err, opened())
	}
	unlock()

	if err := state.Remove(dir.OrcaKept("lane-a")); err != nil {
		t.Fatal(err)
	}
	if err := keepForwards(t.Context(), dir, quiet, noPrune, storageReachable, noOpen, nil, successor{}); err != nil || len(opened()) != 0 {
		t.Errorf("a keep with nothing kept = %v, opened %q; want it to exit at once", err, opened())
	}
}

func (w *firstWorker) keptLanes(names ...string) {
	w.t.Helper()
	w.keptUnder(w.session.Config.Path, names...)
}

func (w *firstWorker) keptUnder(config string, names ...string) {
	w.t.Helper()
	driver := orcaDriver{state: w.session.Config.State(), log: slog.New(slog.DiscardHandler)}
	for _, name := range names {
		w.laneWorkspace(name)
		if err := driver.keep(w.liveTask(name), config); err != nil {
			w.t.Fatal(err)
		}
	}
}

func keysConfig(t *testing.T, keys map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeKeys(t, path, keys)
	return path
}

func writeKeys(t *testing.T, path string, keys map[string]string) {
	t.Helper()
	var named strings.Builder
	for _, key := range slices.Sorted(maps.Keys(keys)) {
		fmt.Fprintf(&named, "    %s: [%q]\n", key, keys[key])
	}
	body := "repository: https://github.com/example/app\nref: main\nprovider: absent\nprofile: lean\ninventory: missing.yaml\nproviders:\n  absent: {}\nworkspace_dirs:\n  absent: /home/agent\nprofiles:\n  lean: {}\norca:\n  keys:\n" + named.String()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestKeepTicksEachKeptTaskOnItsOwnSoAHungReopenHoldsUpNoOtherForward(t *testing.T) {
	ticking(t, 100*time.Millisecond)
	within := reopenWithin
	reopenWithin = 3 * time.Second
	t.Cleanup(func() { reopenWithin = within })
	w := newWarmPool(t)
	opened := stubForwards(t, []string{"lane-hung"}, nil)
	standInKeeper(t)
	w.keptLanes("lane-hung", "lane-next")
	logPath := filepath.Join(t.TempDir(), "keeper.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logFile.Close() })
	logged := func() string {
		raw, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}

	holding(t, w.session.Config.State(), slog.New(slog.NewTextHandler(logFile, nil)))
	if took := eventually(t, 30*time.Second, "lane-next's reopen", func() bool { return slices.Contains(opened(), "lane-next") }); took >= reopenWithin {
		t.Errorf("lane-next's forward opened after %s, behind lane-hung's reopen and its %s deadline; want it on its own tick", took, reopenWithin)
	}
	eventually(t, 30*time.Second, "a log line naming lane-hung's reopen and its deadline", func() bool {
		return strings.Contains(logged(), "workspace=lane-hung") && strings.Contains(logged(), "did not finish within 3s")
	})
	if got := opened(); !slices.Equal(got, []string{"lane-next"}) {
		t.Errorf("forwards opened = %q, want lane-next's alone", got)
	}
}

func TestKeepSendsANewlyKeptWorkerItsFirstGitHubAppTokenWhileAnotherWorkerHangs(t *testing.T) {
	ticking(t, 100*time.Millisecond)
	within := answerWithin
	answerWithin = 5 * time.Second
	t.Cleanup(func() { answerWithin = within })
	w := newWarmPool(t)
	standInKeeper(t)
	bin, sent := t.TempDir(), t.TempDir()
	ssh := "#!/bin/sh\nfor arg do\n  test \"$arg\" = sh && break\n  host=$arg\ndone\ncase \" $* \" in *' -O check '*) exit 0;; esac\ntest \"$host\" = lane-hung && exec sleep 600\ncat >> " + sent + "/\"$host\"\n"
	mint := filepath.Join(bin, "mint")
	for path, body := range map[string]string{filepath.Join(bin, "ssh"): ssh, mint: "#!/bin/sh\necho '{\"token\":\"first\"}'\n"} {
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	keys := keysConfig(t, map[string]string{config.KeyGitHubApp: mint})
	w.keptUnder(keys, "lane-a", "lane-hung")
	received := func(name string) string {
		raw, err := os.ReadFile(filepath.Join(sent, name))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return string(raw)
	}

	holding(t, w.session.Config.State(), slog.New(slog.DiscardHandler), newRenewal(config.KeyGitHubApp, captureAppToken, orca.GitHubAppScript))
	eventually(t, 30*time.Second, "lane-a's first token", func() bool { return received("lane-a") != "" })
	w.keptUnder(keys, "lane-new")
	if took := eventually(t, 30*time.Second, "lane-new's first token", func() bool { return received("lane-new") != "" }); took >= answerWithin {
		t.Errorf("lane-new got its first token %s after it was kept, behind lane-hung's send and its %s deadline; want it on its own tick", took, answerWithin)
	}
	if a, n, h := received("lane-a"), received("lane-new"), received("lane-hung"); a != `{"token":"first"}` || n != a || h != "" {
		t.Errorf("lane-a got %q, lane-new %q and lane-hung %q; want the token once each on the two that answer", a, n, h)
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
	k := newKeeper(dir, slog.New(slog.NewTextHandler(&logged, nil)), storageGoneFor("lane-dead"), noOpen, nil)
	dead, down, live := newKeptTask("lane-dead"), newKeptTask("lane-down"), newKeptTask("lane-live")

	if !tick(t, k, dead) {
		t.Fatal("the first tick of lane-dead ended it; want it kept, since one look at a gone storage releases nothing")
	}
	if strings.Contains(logged.String(), "lane-dead") {
		t.Errorf("the first look at lane-dead's gone storage logged %q, want nothing until it is released", logged.String())
	}
	if tick(t, k, dead) {
		t.Fatal("the second tick of lane-dead kept it; want it released")
	}
	if _, err := os.Stat(dir.OrcaKept("lane-dead")); !os.IsNotExist(err) {
		t.Errorf("lane-dead is still kept: %v", err)
	}
	if tick(t, k, dead) {
		t.Error("a tick of the released lane-dead kept it; want its ticks to end")
	}
	for range 3 {
		if !tick(t, k, down) || !tick(t, k, live) {
			t.Fatal("a tick of lane-down or lane-live ended it; want both kept")
		}
	}
	if n := strings.Count(logged.String(), "released a kept forward"); n != 1 || !strings.Contains(logged.String(), "workspace=lane-dead") {
		t.Errorf("logged %d releases in %q, want lane-dead's once", n, logged.String())
	}
	if n := strings.Count(logged.String(), "did not reopen, and the next pass tries again\" workspace=lane-down"); n != 3 {
		t.Errorf("lane-down, whose storage answers, logged %d failed reopens over three ticks, want 3: %q", n, logged.String())
	}
	if got := opened(); !slices.Equal(got, []string{"lane-live"}) {
		t.Errorf("forwards opened = %q, want lane-live's alone", got)
	}
}

func TestKeepSendsEachKeptWorkerEveryGitHubAppTokenTheMacMintsOnceItsForwardIsUp(t *testing.T) {
	w := newWarmPool(t)
	standInKeeper(t)
	dir := w.session.Config.State()
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
	w.keptUnder(keysConfig(t, map[string]string{config.KeyGitHubApp: mint}), "lane-a", "lane-b")
	received := func(name string) string {
		raw, err := os.ReadFile(filepath.Join(sent, name))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return string(raw)
	}
	k := newKeeper(dir, slog.New(slog.DiscardHandler), storageReachable, noOpen, []*renewal{newRenewal(config.KeyGitHubApp, captureAppToken, orca.GitHubAppScript)})
	a, b := newKeptTask("lane-a"), newKeptTask("lane-b")
	pass := func(line string) {
		t.Helper()
		if err := os.WriteFile(minted, []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
		keepPass(t, k, a, b)
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
	keepPass(t, k, a, b)
	if b := received("lane-b"); b != `{"token":"first"}{"token":"second"}` {
		t.Errorf("after a failed mint lane-b got %q; want nothing new sent", b)
	}
}

type awsWorkers struct {
	t         *testing.T
	bin, sent string
}

func stubAWSWorkers(t *testing.T) awsWorkers {
	t.Helper()
	w := awsWorkers{t: t, bin: t.TempDir(), sent: t.TempDir()}
	ssh := "#!/bin/sh\nfor arg do\n  test \"$arg\" = sh && break\n  host=$arg\ndone\ncase \" $* \" in *' -O check '*) exit 0;; esac\ncat >> " + w.sent + "/\"$host\"\necho >> " + w.sent + "/\"$host\"\n"
	if err := os.WriteFile(filepath.Join(w.bin, "ssh"), []byte(ssh), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", w.bin+":"+os.Getenv("PATH"))
	return w
}

func (w awsWorkers) mints(name, session string) string {
	w.t.Helper()
	path := filepath.Join(w.bin, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho >> "+path+".runs\ncat "+path+".out\n"), 0o700); err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(path+".out", []byte(session), 0o600); err != nil {
		w.t.Fatal(err)
	}
	return path
}

func (w awsWorkers) runs(name string) int {
	w.t.Helper()
	raw, err := os.ReadFile(filepath.Join(w.bin, name+".runs"))
	if err != nil && !os.IsNotExist(err) {
		w.t.Fatal(err)
	}
	return strings.Count(string(raw), "\n")
}

func (w awsWorkers) received(name string) string {
	w.t.Helper()
	raw, err := os.ReadFile(filepath.Join(w.sent, name))
	if err != nil && !os.IsNotExist(err) {
		w.t.Fatal(err)
	}
	return string(raw)
}

func awsSession(id string, left time.Duration) string {
	return fmt.Sprintf("{\n  \"Version\": 1,\n  \"AccessKeyId\": %q,\n  \"SecretAccessKey\": \"secret\",\n  \"SessionToken\": \"token\",\n  \"Expiration\": %q\n}\n", id, time.Now().Add(left).UTC().Format(time.RFC3339))
}

func awsLine(t *testing.T, session string) string {
	t.Helper()
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(session)); err != nil {
		t.Fatal(err)
	}
	return compact.String() + "\n"
}

func TestKeepSendsEachKeptWorkerANewAWSSessionOnceTheOneItHoldsHasUnderAnHourLeft(t *testing.T) {
	w := newWarmPool(t)
	standInKeeper(t)
	dir := w.session.Config.State()
	workers := stubAWSWorkers(t)
	w.keptUnder(keysConfig(t, map[string]string{config.KeyAWS: workers.mints("mint", "")}), "lane-a", "lane-b")
	k := newKeeper(dir, slog.New(slog.DiscardHandler), storageReachable, noOpen, []*renewal{newRenewal(config.KeyAWS, captureAWS, orca.AWSScript)})
	a, b := newKeptTask("lane-a"), newKeptTask("lane-b")
	pass := func(session string) {
		t.Helper()
		workers.mints("mint", session)
		keepPass(t, k, a, b)
	}

	expiring, fresh, unused := awsSession("ASIA-expiring", 50*time.Minute), awsSession("ASIA-fresh", 4*time.Hour), awsSession("ASIA-unused", 4*time.Hour)
	pass(expiring)
	pass(fresh)
	pass(unused)
	want := awsLine(t, expiring) + awsLine(t, fresh)
	if a, b := workers.received("lane-a"), workers.received("lane-b"); a != want || b != want {
		t.Errorf("lane-a got %q and lane-b %q; want the session with 50 minutes left, then the fresh one, each once", a, b)
	}
	if n := workers.runs("mint"); n != 2 {
		t.Errorf("the mint ran %d times over three passes, want twice: once for the first session and once because it had under an hour left, not while the fresh one has four hours", n)
	}
}

func TestKeepRenewsEachTaskWithTheKeysOfTheConfigThatKeptItReadAgainEachPass(t *testing.T) {
	w := newWarmPool(t)
	standInKeeper(t)
	dir := w.session.Config.State()
	workers := stubAWSWorkers(t)
	first, second := awsSession("ASIA-first", 4*time.Hour), awsSession("ASIA-second", 4*time.Hour)
	withAWS := keysConfig(t, map[string]string{config.KeyAWS: workers.mints("mint-first", first)})
	withoutAWS := keysConfig(t, nil)
	w.keptUnder(withAWS, "lane-a", "lane-b")
	w.keptUnder(withoutAWS, "lane-c")
	w.keptUnder("", "lane-old")
	k := newKeeper(dir, slog.New(slog.DiscardHandler), storageReachable, noOpen, []*renewal{newRenewal(config.KeyAWS, captureAWS, orca.AWSScript)})
	lanes := []*keptTask{newKeptTask("lane-a"), newKeptTask("lane-b"), newKeptTask("lane-c"), newKeptTask("lane-old")}

	keepPass(t, k, lanes...)
	if a, b, c := workers.received("lane-a"), workers.received("lane-b"), workers.received("lane-c"); a != awsLine(t, first) || b != a || c != "" {
		t.Fatalf("lane-a got %q, lane-b %q and lane-c %q; want the two lanes kept under the config naming orca.keys.aws to get its session, and lane-c, whose config names none, nothing", a, b, c)
	}
	if n := workers.runs("mint-first"); n != 1 {
		t.Errorf("two lanes kept under one config ran its orca.keys.aws command %d times, want once", n)
	}

	writeKeys(t, withoutAWS, map[string]string{config.KeyAWS: workers.mints("mint-second", second)})
	keepPass(t, k, lanes...)
	if c := workers.received("lane-c"); c != awsLine(t, second) {
		t.Errorf("once its config named orca.keys.aws, lane-c got %q; want that command's session on the same keep's next pass", c)
	}
	if a := workers.received("lane-a"); a != awsLine(t, first) {
		t.Errorf("lane-a got %q; want its own config's session once, untouched by lane-c's config", a)
	}

	if err := os.Remove(withAWS); err != nil {
		t.Fatal(err)
	}
	keepPass(t, k, lanes...)
	if old := workers.received("lane-old"); old != "" {
		t.Errorf("lane-old, kept by a release that recorded no config, got %q; want nothing", old)
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
		if err := keepForwards(t.Context(), dir, quiet, noPrune, storageReachable, noOpen, nil, offered); err != nil {
			t.Fatalf("a %s keep beside the running one = %v; want it to exit at once", offered.Version, err)
		}
	}
	unlock()
	if next, err := readSuccessor(dir); err != nil || next == nil || *next != newer {
		t.Fatalf("recorded successor = %+v, %v; want the 0.27.10 keep, not the older or dev one offered after it", next, err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := keepForwards(ctx, dir, quiet, noPrune, storageReachable, noOpen, nil, running); !errors.Is(err, context.DeadlineExceeded) {
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
	if err := keepForwards(ctx, dir, quiet, noPrune, storageReachable, noOpen, nil, running); !errors.Is(err, context.DeadlineExceeded) || len(started) != 1 {
		t.Errorf("the 0.27.9 keep beside a recorded 0.27.8 successor = %v, started %+v; want no handoff to an older release", err, started)
	}
}
