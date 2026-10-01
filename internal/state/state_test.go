package state

import (
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
