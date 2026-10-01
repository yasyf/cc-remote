package workspace

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yasyf/cc-remote/internal/budget"
	"github.com/yasyf/cc-remote/internal/config"
	"github.com/yasyf/cc-remote/internal/state"
)

var poolEpoch = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

func testPool(t *testing.T, spares int, capUSD float64) Pool {
	t.Helper()
	store := budget.Store{Path: filepath.Join(t.TempDir(), "ledger.json")}
	if err := store.Init(poolEpoch); err != nil {
		t.Fatal(err)
	}
	return Pool{
		Provider:    "fake",
		Profile:     "lean",
		Fingerprint: "f00d",
		Spares:      spares,
		Rate:        budget.Rate{HourlyUSD: 1},
		Estimate:    3,
		Guard:       budget.Guard{CapUSD: capUSD, ReserveUSD: 10},
		Ledger:      store,
	}
}

func prepared(t *testing.T, pool Pool, names ...string) {
	t.Helper()
	for _, name := range names {
		reserved, err := pool.Reserve(name, os.Getpid(), poolEpoch)
		if err != nil || !reserved {
			t.Fatalf("reserving %s: %v, %v", name, reserved, err)
		}
		if err := pool.MarkReady(name, poolEpoch); err != nil {
			t.Fatal(err)
		}
	}
}

func readLedger(t *testing.T, pool Pool) *budget.Ledger {
	t.Helper()
	ledger, err := pool.Ledger.Read()
	if err != nil {
		t.Fatal(err)
	}
	return ledger
}

func readSpares(t *testing.T, pool Pool) budget.Spares {
	t.Helper()
	spares, err := pool.Ledger.ReadSpares()
	if err != nil {
		t.Fatal(err)
	}
	return spares
}

func TestReserveFillsThePoolToItsTarget(t *testing.T) {
	pool := testPool(t, 2, 100)
	reserved := make([]bool, 0, 3)
	for _, name := range []string{"a", "b", "c"} {
		ok, err := pool.Reserve(name, os.Getpid(), poolEpoch)
		if err != nil {
			t.Fatal(err)
		}
		reserved = append(reserved, ok)
	}
	if fmt.Sprint(reserved) != "[true true false]" {
		t.Errorf("reserved %v against a target of 2", reserved)
	}
	ledger := readLedger(t, pool)
	if spares := readSpares(t, pool); len(spares) != 2 || ledger.Resources["c"] != nil {
		t.Errorf("the pool holds %d spares and c = %+v", len(spares), ledger.Resources["c"])
	}
	if near := ledger.Committed(poolEpoch); near != 6 {
		t.Errorf("two preparing spares commit $%.2f, want their $6 reservations", near)
	}
}

func TestReserveKeepsTheCleanupReserve(t *testing.T) {
	pool := testPool(t, 5, 15)
	prepared(t, pool, "a")
	if _, err := pool.Reserve("b", os.Getpid(), poolEpoch); err != nil {
		t.Fatal(err)
	}
	reserved, err := pool.Reserve("c", os.Getpid(), poolEpoch)
	var over *budget.OverBudgetError
	if reserved || !errors.As(err, &over) {
		t.Fatalf("a spare past the $5 left above the reserve was reserved: %v, %v", reserved, err)
	}
	if readLedger(t, pool).Resources["c"] != nil {
		t.Error("the refused spare reached the ledger")
	}
}

func TestAssignClaimsAReadySpareAndReservesItsTrial(t *testing.T) {
	pool := testPool(t, 1, 100)
	prepared(t, pool, "spare-a")
	name, assignment, err := pool.Assign("ws-req", poolEpoch.Add(time.Hour))
	if err != nil || assignment != NewClaim || name != "spare-a" {
		t.Fatalf("assign = %q, %v, %v", name, assignment, err)
	}
	ledger := readLedger(t, pool)
	running := ledger.Resources[name].Running
	if len(running) != 2 || running[1].End != nil || running[1].ReservedUSD != 3 {
		t.Errorf("the claimed spare's intervals are %+v", running)
	}
	if ledger.Resources["ws-req"] != nil {
		t.Error("a claim also started a fresh resource")
	}
}

func TestAssignFallsThroughToAFreshCreate(t *testing.T) {
	pool := testPool(t, 1, 100)
	name, assignment, err := pool.Assign("ws-req", poolEpoch)
	if err != nil || assignment != FreshCreate || name != "ws-req" {
		t.Fatalf("assign on an empty pool = %q, %v, %v", name, assignment, err)
	}
	if resource := readLedger(t, pool).Resources[name]; resource == nil || len(resource.Running) != 1 {
		t.Errorf("the fresh create's ledger entry is %+v", resource)
	}
}

func TestAssignRefusesARequestThatAlreadyHasAWorkspace(t *testing.T) {
	pool := testPool(t, 1, 100)
	if _, _, err := pool.Assign("ws-req", poolEpoch); err != nil {
		t.Fatal(err)
	}
	prepared(t, pool, "spare-a")
	for _, at := range []time.Duration{0, time.Hour} {
		if at > 0 {
			if err := pool.Stop("ws-req", poolEpoch.Add(at)); err != nil {
				t.Fatal(err)
			}
		}
		if name, assignment, err := pool.Assign("ws-req", poolEpoch.Add(at+time.Minute)); err == nil {
			t.Fatalf("a retry over an existing workspace got %q, %v", name, assignment)
		}
	}
	if spare := readSpares(t, pool)["spare-a"]; spare.State != budget.Ready {
		t.Errorf("the refused retry took a spare: %+v", spare)
	}
}

func TestAssignRefusesOverBudgetWithoutClaiming(t *testing.T) {
	pool := testPool(t, 2, 15)
	prepared(t, pool, "spare-a")
	if reserved, err := pool.Reserve("spare-b", os.Getpid(), poolEpoch); err != nil || !reserved {
		t.Fatal(reserved, err)
	}
	if _, _, err := pool.Assign("ws-req", poolEpoch); err == nil {
		t.Fatal("a $3 trial fit in the $2 above the reserve")
	}
	if spare := readSpares(t, pool)["spare-a"]; spare.State != budget.Ready {
		t.Errorf("the refused claim left the spare %s", spare.State)
	}
}

func TestConcurrentCreatesClaimDistinctSpares(t *testing.T) {
	pool := testPool(t, 3, 100)
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

func TestARetriedClaimReattachesWithoutFreshBudget(t *testing.T) {
	pool := testPool(t, 2, 16)
	prepared(t, pool, "spare-a")
	if _, assignment, err := pool.Assign("ws-req", poolEpoch); err != nil || assignment != NewClaim {
		t.Fatal(assignment, err)
	}
	if err := pool.Activate("spare-a", poolEpoch); err != nil {
		t.Fatal(err)
	}
	if reserved, err := pool.Reserve("spare-b", os.Getpid(), poolEpoch); err != nil || !reserved {
		t.Fatal(reserved, err)
	}
	name, assignment, err := pool.Assign("ws-req", poolEpoch.Add(time.Minute))
	if err != nil || assignment != Reclaim || name != "spare-a" {
		t.Errorf("the retry with no headroom left got %q, %v, %v", name, assignment, err)
	}
}

func TestDrainRemovesStaleSparesAndCrashedPreparations(t *testing.T) {
	pool := testPool(t, 4, 100)
	prepared(t, pool, "current")
	stale := pool
	stale.Fingerprint = "0ld"
	prepared(t, stale, "stale")
	if reserved, err := pool.Reserve("preparing", os.Getpid(), poolEpoch); err != nil || !reserved {
		t.Fatal(reserved, err)
	}
	crashed := exec.Command("true")
	if err := crashed.Run(); err != nil {
		t.Fatal(err)
	}
	if reserved, err := pool.Reserve("crashed", crashed.Process.Pid, poolEpoch); err != nil || !reserved {
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
budget:
  cap_usd: 100
  reserve_usd: 10
  trial_hours: 3
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
	rate := budget.Rate{HourlyUSD: 1}
	stamp := strings.Repeat("a", 64)
	base, err := NewPool(cfg, "fake", "lean", rate, stamp)
	if err != nil {
		t.Fatal(err)
	}
	if base.Spares != 1 || base.Estimate != 3 || base.Guard.CapUSD != 100 || base.Ledger.Path != cfg.State().Ledger("default") {
		t.Errorf("pool = %+v", base)
	}
	fingerprint := func() string {
		t.Helper()
		pool, err := NewPool(cfg, "fake", "lean", rate, stamp)
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
