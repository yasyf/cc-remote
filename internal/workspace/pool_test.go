package workspace

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yasyf/cc-remote/internal/config"
	sparestate "github.com/yasyf/cc-remote/internal/spare"
	"github.com/yasyf/cc-remote/internal/state"
)

var poolEpoch = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

func testPool(t *testing.T, spares int) Pool {
	t.Helper()
	return Pool{Provider: "fake", Profile: "lean", Fingerprint: "f00d", Spares: spares, Store: sparestate.Store{Path: filepath.Join(t.TempDir(), "spares.json")}}
}

func prepared(t *testing.T, pool Pool, names ...string) {
	t.Helper()
	for _, name := range names {
		reserved, err := pool.BeginPrepare(name, os.Getpid())
		if err != nil || !reserved {
			t.Fatalf("reserving %s: %v, %v", name, reserved, err)
		}
		if err := pool.MarkReady(name, poolEpoch); err != nil {
			t.Fatal(err)
		}
	}
}

func readSpares(t *testing.T, pool Pool) sparestate.Spares {
	t.Helper()
	spares, err := pool.Store.Read()
	if err != nil {
		t.Fatal(err)
	}
	return spares
}

func TestPreparationFillsThePoolToItsTarget(t *testing.T) {
	pool := testPool(t, 2)
	reserved := make([]bool, 0, 3)
	for _, name := range []string{"a", "b", "c"} {
		ok, err := pool.BeginPrepare(name, os.Getpid())
		if err != nil {
			t.Fatal(err)
		}
		reserved = append(reserved, ok)
	}
	if fmt.Sprint(reserved) != "[true true false]" {
		t.Errorf("reserved %v against a target of 2", reserved)
	}
	if spares := readSpares(t, pool); len(spares) != 2 || spares["c"] != nil {
		t.Errorf("spares = %v", spares)
	}
}

func TestAssignClaimsAReadySpare(t *testing.T) {
	pool := testPool(t, 1)
	prepared(t, pool, "spare-a")
	name, assignment, err := pool.Assign("ws-req", poolEpoch.Add(time.Hour))
	if err != nil || assignment != NewClaim || name != "spare-a" {
		t.Fatalf("assign = %q, %v, %v", name, assignment, err)
	}
	if item := readSpares(t, pool)[name]; item.State != sparestate.Claimed || item.Request != "ws-req" {
		t.Errorf("claim = %+v", item)
	}
}

func TestAssignFallsThroughToAFreshCreate(t *testing.T) {
	pool := testPool(t, 1)
	name, assignment, err := pool.Assign("ws-req", poolEpoch)
	if err != nil || assignment != FreshCreate || name != "ws-req" {
		t.Fatalf("assign on an empty pool = %q, %v, %v", name, assignment, err)
	}
}

func TestAssignPreservesExistingSpareMachineNames(t *testing.T) {
	for _, existing := range []sparestate.State{sparestate.Preparing, sparestate.Ready, sparestate.Claimed, sparestate.Draining} {
		t.Run(string(existing), func(t *testing.T) {
			pool := testPool(t, 1)
			prepared(t, pool, "existing")
			if err := pool.Store.Update(func(spares sparestate.Spares) error {
				spares["existing"].State = existing
				spares["existing"].Fingerprint = "another-pool"
				spares["existing"].Request = "another-workspace"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before := *readSpares(t, pool)["existing"]
			if _, _, err := pool.Assign("existing", poolEpoch); err == nil || !strings.Contains(err.Error(), "already exists as a spare") {
				t.Fatalf("Assign error = %v", err)
			}
			after := *readSpares(t, pool)["existing"]
			if before.State != after.State || before.Fingerprint != after.Fingerprint || before.Request != after.Request {
				t.Fatalf("Assign changed spare from %+v to %+v", before, after)
			}
		})
	}
}

func TestConcurrentCreatesClaimDistinctSpares(t *testing.T) {
	pool := testPool(t, 3)
	prepared(t, pool, "a", "b", "c")
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := map[string][]string{}
	for i := range 6 {
		wg.Go(func() {
			request := fmt.Sprintf("ws-req-%d", i)
			name, assignment, err := pool.Assign(request, poolEpoch)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if assignment == NewClaim {
				got[name] = append(got[name], request)
			} else if name != request {
				t.Errorf("%s fell through as %s", request, name)
			}
		})
	}
	wg.Wait()
	if len(got) != 3 {
		t.Errorf("claims = %v, want each of a, b and c once", got)
	}
	for name, requests := range got {
		if len(requests) != 1 {
			t.Errorf("%s went to %v", name, requests)
		}
	}
}

func TestARetriedClaimReattaches(t *testing.T) {
	pool := testPool(t, 2)
	prepared(t, pool, "spare-a")
	if _, assignment, err := pool.Assign("ws-req", poolEpoch); err != nil || assignment != NewClaim {
		t.Fatal(assignment, err)
	}
	if err := pool.Activate("spare-a", poolEpoch); err != nil {
		t.Fatal(err)
	}
	if reserved, err := pool.BeginPrepare("spare-b", os.Getpid()); err != nil || !reserved {
		t.Fatal(reserved, err)
	}
	name, assignment, err := pool.Assign("ws-req", poolEpoch.Add(time.Minute))
	if err != nil || assignment != Reclaim || name != "spare-a" {
		t.Errorf("the retry with no headroom left got %q, %v, %v", name, assignment, err)
	}
}

func TestDrainRemovesStaleSparesAndCrashedPreparations(t *testing.T) {
	pool := testPool(t, 4)
	prepared(t, pool, "current")
	stale := pool
	stale.Fingerprint = "0ld"
	prepared(t, stale, "stale")
	if reserved, err := pool.BeginPrepare("preparing", os.Getpid()); err != nil || !reserved {
		t.Fatal(reserved, err)
	}
	crashed := exec.Command("true")
	if err := crashed.Run(); err != nil {
		t.Fatal(err)
	}
	if reserved, err := pool.BeginPrepare("crashed", crashed.Process.Pid); err != nil || !reserved {
		t.Fatal(reserved, err)
	}
	drained, err := pool.Drain(false)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(drained) != "[crashed stale]" {
		t.Errorf("drained %v, want the crashed preparation and the stale spare", drained)
	}
	if all, err := pool.Drain(true); err != nil || fmt.Sprint(all) != "[crashed current stale]" {
		t.Errorf("drain --all took %v, %v; want the unfinished drains retried and a live preparation left alone", all, err)
	}
}

func poolConfig(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "inventory.yaml"), []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(`
repository: https://github.com/example/app
ref: main
provider: fake
profile: lean
providers:
  fake: { org: o }
workspace_dirs:
  fake: /home/fake
profiles:
  lean:
    prepare: ["mise install"]
    warm: ["yarn install"]
    machine:
      fake: { size: l }
spares:
  fake: { lean: 1 }
inventory: inventory.yaml
`))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Path = filepath.Join(dir, "config.yaml")
	cfg.StateDir = t.TempDir()
	return cfg
}

func TestTheFingerprintFollowsWhatASpareHolds(t *testing.T) {
	cfg := poolConfig(t)
	stamp := strings.Repeat("a", 64)
	base, err := NewPool(cfg, "fake", "lean", stamp)
	if err != nil {
		t.Fatal(err)
	}
	if base.Spares != 1 || base.Store.Path != cfg.State().Spares() {
		t.Errorf("pool = %+v", base)
	}
	fingerprint := func() string {
		t.Helper()
		pool, err := NewPool(cfg, "fake", "lean", stamp)
		if err != nil {
			t.Fatal(err)
		}
		return pool.Fingerprint
	}
	cfg.Spares["fake"]["lean"] = 3
	if fingerprint() != base.Fingerprint {
		t.Error("resizing the pool invalidated its spares")
	}
	stamp = strings.Repeat("b", 64)
	bumped := fingerprint()
	if bumped == base.Fingerprint {
		t.Error("a new tool stamp kept the old fingerprint")
	}
	profile := cfg.Profiles["lean"]
	profile.Warm = []string{"yarn install", "go mod download"}
	cfg.Profiles["lean"] = profile
	warmed := fingerprint()
	if warmed == bumped {
		t.Error("a new warm step kept the old fingerprint")
	}
	profile.Machine["fake"] = config.Machine{Size: "xl"}
	cfg.Profiles["lean"] = profile
	shaped := fingerprint()
	if shaped == warmed {
		t.Error("a new machine shape kept the old fingerprint, though acquiring an old machine never resizes it")
	}
	cfg.Providers["fake"].Content[1].Value = "other"
	if fingerprint() == shaped {
		t.Error("a changed provider section kept the old fingerprint")
	}
	if name := base.SpareName(); !strings.HasPrefix(name, "cc-remote-spare-lean-"+base.Fingerprint[:12]+"-") || state.ValidateName(name) != nil {
		t.Errorf("spare name %q", name)
	}
}

func TestSpareNamesStayWithinTheLimitForTheLongestProfile(t *testing.T) {
	for _, profile := range []string{"lean", "a-profile-of-twenty0", "trailing-dash-at-16-x"} {
		pool := Pool{Profile: profile, Fingerprint: strings.Repeat("f", 64)}
		name := pool.SpareName()
		if err := state.ValidateName(name); err != nil {
			t.Errorf("%s: %v", profile, err)
		}
		if !strings.HasSuffix(name[:len(name)-9], "-"+pool.Fingerprint[:12]) || len(name[len(name)-8:]) != 8 || !strings.HasPrefix(name, "cc-remote-spare-"+profile[:min(len(profile), 4)]) {
			t.Errorf("%s: spare name %q lost its fingerprint or random suffix", profile, name)
		}
	}
}
