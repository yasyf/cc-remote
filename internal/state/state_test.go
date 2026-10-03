package state

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultFollowsXDGStateHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/xdg")
	if got := Default(); got != Dir("/tmp/xdg/cc-remote") {
		t.Errorf("Default() = %q", got)
	}
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/home/u")
	if got := Default(); got != Dir("/home/u/.local/state/cc-remote") {
		t.Errorf("Default() = %q", got)
	}
}

func TestOrcaControlUsesDistinctShortPrivateDirectories(t *testing.T) {
	seen := map[string]bool{}
	for range 2 {
		control, err := NewOrcaControl()
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Dir(control)
		t.Cleanup(func() { _ = os.Remove(dir) })
		if seen[control] {
			t.Fatalf("control path reused: %s", control)
		}
		seen[control] = true
		expanded := strings.ReplaceAll(control, "%C", strings.Repeat("a", 40))
		if filepath.Dir(dir) != "/tmp" || len(expanded)+17 >= 104 {
			t.Errorf("control path leaves no room for the SSH temporary socket: %s", expanded)
		}
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Errorf("control directory = %v, %v", info, err)
		}
	}
}

func TestEnsureOrcaControlRestoresOnlyItsPrivateParent(t *testing.T) {
	control, err := NewOrcaControl()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(control)
	t.Cleanup(func() { _ = os.Remove(dir) })
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := EnsureOrcaControl(control); err != nil {
		t.Fatal(err)
	}
	socket := strings.ReplaceAll(control, "%C", strings.Repeat("a", 40))
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	before, err := os.Lstat(socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureOrcaControl(control); err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(socket)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("existing control socket changed: %v", err)
	}
}

func TestEnsureOrcaControlRefusesUnsafeParents(t *testing.T) {
	for _, kind := range []string{"symlink", "public", "file"} {
		t.Run(kind, func(t *testing.T) {
			control, err := NewOrcaControl()
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Dir(control)
			t.Cleanup(func() { _ = os.Remove(dir) })
			if err := os.Remove(dir); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "symlink":
				err = os.Symlink(t.TempDir(), dir)
			case "public":
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				err = os.Chmod(dir, 0o755)
			case "file":
				err = os.WriteFile(dir, nil, 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := EnsureOrcaControl(control); err == nil {
				t.Fatalf("accepted %s control parent", kind)
			}
		})
	}
	if err := EnsureOrcaControl(filepath.Join(t.TempDir(), "%C")); err == nil {
		t.Fatal("accepted a control path outside the private control namespace")
	}
}

func TestValidateNameBoundsTheProviderNameLimit(t *testing.T) {
	tests := []struct {
		name string
		ok   bool
	}{
		{"ws-1", true},
		{"a", true},
		{strings.Repeat("a", NameLimit), true},
		{strings.Repeat("a", NameLimit+1), false},
		{"-leading", false},
		{"Upper", false},
		{"has space", false},
		{"", false},
		{"dot.name", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateName(tt.name); (err == nil) != tt.ok {
				t.Errorf("ValidateName(%q) = %v, want ok=%v", tt.name, err, tt.ok)
			}
		})
	}
}

func TestSaveWritesAtomicallyAndLoadReadsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "record.json")
	type record struct {
		Name string `json:"name"`
		N    int    `json:"n"`
	}
	var missing record
	if found, err := Load(path, &missing); err != nil || found {
		t.Fatalf("a missing record loaded: %v %v", found, err)
	}
	if err := Save(path, record{Name: "ws", N: 2}); err != nil {
		t.Fatal(err)
	}
	var got record
	if found, err := Load(path, &got); err != nil || !found || got != (record{Name: "ws", N: 2}) {
		t.Errorf("loaded %+v, %v, %v", got, found, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Errorf("a temporary file survived: %s", entry.Name())
		}
	}
	if err := Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := Remove(path); err != nil {
		t.Errorf("removing an absent record: %v", err)
	}
	if found, _ := Load(path, &got); found {
		t.Error("the record survived its removal")
	}
}

func TestLoadRefusesMalformedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	var into map[string]any
	if _, err := Load(path, &into); err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("err = %v", err)
	}
}

func TestHoldSerializesHoldersOfOneNameOnly(t *testing.T) {
	dir := Dir(t.TempDir())
	first, err := dir.Hold("ws-1")
	if err != nil {
		t.Fatal(err)
	}
	other, err := dir.Hold("ws-2")
	if err != nil {
		t.Fatalf("another name waited on the first: %v", err)
	}
	other.Release()
	second := make(chan *Held)
	go func() {
		held, err := dir.Hold("ws-1")
		if err != nil {
			t.Error(err)
		}
		second <- held
	}()
	select {
	case held := <-second:
		held.Release()
		t.Fatal("a second holder got the name while the first still held it")
	case <-time.After(100 * time.Millisecond):
	}
	first.Release()
	select {
	case held := <-second:
		held.Release()
	case <-time.After(2 * time.Second):
		t.Fatal("the second holder never got the name after the first released it")
	}
	if _, err := dir.Hold("Bad Name"); err == nil {
		t.Error("an invalid name was held")
	}
}

func TestTryLockReportsAHeldLockWithoutWaiting(t *testing.T) {
	dir := Dir(t.TempDir())
	path := dir.OrcaGatewayLock("ws-1", "inst1")
	if want := filepath.Join(string(dir), "orca", "ws-1.inst1.gateway.lock"); path != want {
		t.Errorf("gateway lock = %s, want %s", path, want)
	}
	if got, want := dir.OrcaLease("ws-1", "inst1"), filepath.Join(string(dir), "orca", "ws-1.inst1.lease.json"); got != want {
		t.Errorf("lease = %s, want %s", got, want)
	}
	unlock, held, err := TryLock(path)
	if err != nil || !held {
		t.Fatalf("first TryLock = %t, %v", held, err)
	}
	started := time.Now()
	if again, held, err := TryLock(path); err != nil || held || again != nil {
		t.Errorf("second TryLock = %t, %v; want it reported as held", held, err)
	}
	if waited := time.Since(started); waited > time.Second {
		t.Errorf("TryLock waited %s on a held lock", waited)
	}
	other, held, err := TryLock(dir.OrcaGatewayLock("ws-1", "inst2"))
	if err != nil || !held {
		t.Fatalf("another instance's lock = %t, %v; want it free", held, err)
	}
	other()
	unlock()
	relock, held, err := TryLock(path)
	if err != nil || !held {
		t.Fatalf("TryLock after release = %t, %v", held, err)
	}
	relock()
	if info, err := os.Stat(filepath.Dir(path)); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("lock directory = %v, %v", info, err)
	}
}
