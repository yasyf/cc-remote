package cli

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func TestPrimaryCheckout(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	primary := filepath.Join(root, "project")
	linked := filepath.Join(root, "linked")
	separate := filepath.Join(root, "separate")
	git(t, root, "init", primary)
	git(t, primary, "commit", "--allow-empty", "-m", "init")
	git(t, primary, "worktree", "add", linked)
	git(t, root, "init", "--separate-git-dir", filepath.Join(root, "metadata"), separate)

	tests := []struct {
		name string
		cwd  string
		want string
	}{
		{"primary checkout", primary, primary},
		{"linked worktree", linked, primary},
		{"separate git dir", separate, separate},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(tt.cwd)
			got, err := primaryCheckout(context.Background())
			if err != nil {
				t.Fatalf("primaryCheckout() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("primaryCheckout() = %q, want %q", got, tt.want)
			}
		})
	}
}
