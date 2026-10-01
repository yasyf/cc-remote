package identity

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSSHKeyPairIsGeneratedOnceAndRemovedWhole(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ssh")
	pair, err := SSHKeyPair(context.Background(), dir, "ws-1")
	if err != nil {
		t.Fatal(err)
	}
	public, err := pair.PublicKey()
	if err != nil || !strings.HasPrefix(string(public), "ssh-ed25519 ") || !strings.Contains(string(public), "cc-remote ws-1") {
		t.Errorf("public key = %q, %v", public, err)
	}
	info, err := os.Stat(pair.Private)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("private key mode = %v, %v", info.Mode(), err)
	}
	again, err := SSHKeyPair(context.Background(), dir, "ws-1")
	if err != nil || again != pair {
		t.Errorf("a second call regenerated the key: %+v, %v", again, err)
	}
	if public2, _ := again.PublicKey(); string(public2) != string(public) {
		t.Error("the key changed between calls")
	}
	if err := pair.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pair.Public); !errors.Is(err, os.ErrNotExist) {
		t.Error("the public key survived Remove")
	}
	if err := pair.Remove(); err != nil {
		t.Errorf("removing an absent pair: %v", err)
	}
}
