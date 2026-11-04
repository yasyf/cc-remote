package images

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type localDiskBox struct {
	home, disk, boot, store, mounts, services string
	env                                       []string
}

func newLocalDiskBox(t *testing.T) localDiskBox {
	t.Helper()
	dir := t.TempDir()
	box := localDiskBox{
		home:     filepath.Join(dir, "home", "agent"),
		disk:     filepath.Join(dir, "disk"),
		boot:     filepath.Join(dir, "opt", "local-disk.sh"),
		store:    filepath.Join(dir, "store"),
		mounts:   filepath.Join(dir, "mounts"),
		services: filepath.Join(dir, "services"),
	}
	bin := filepath.Join(dir, "bin")
	for _, made := range []string{box.home, box.disk, bin} {
		if err := os.MkdirAll(made, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(box.home, ".bashrc"), []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	mounted := filepath.Join(dir, "mounted")
	for name, script := range map[string]string{
		"getent":     "#!/bin/sh\necho \"agent:x:1:1::" + box.home + ":/bin/sh\"\n",
		"mountpoint": "#!/bin/sh\n[ -e " + mounted + " ]\n",
		"mount":      "#!/bin/sh\necho \"$@\" >> " + box.mounts + "\ntouch " + mounted + "\n",
		"sprite-env": "#!/bin/sh\necho \"$@\" >> " + box.services + "\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	box.env = append(os.Environ(), "SUDO_USER=agent", "PATH="+bin+":"+os.Getenv("PATH"))
	return box
}

func (b localDiskBox) move(t *testing.T) {
	t.Helper()
	script := strings.Replace(moveToLocalDisk, "boot=/opt/cc-remote/local-disk.sh\n", "boot="+b.boot+"\n", 1)
	script = strings.Replace(script, "store=/var/lib/cc-remote\n", "store="+b.store+"\n", 1)
	err := Scripts{}.MoveToLocalDisk(context.Background(), func(_ context.Context, argv []string, _ io.Reader) error {
		if want := []string{"sudo", "--preserve-env=PATH", "bash", "-s", b.disk}; !slices.Equal(argv, want) {
			t.Errorf("ran %q, want %q", argv, want)
		}
		cmd := exec.Command("bash", "-s", b.disk)
		cmd.Stdin, cmd.Env = strings.NewReader(script), b.env
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			t.Fatalf("the move failed: %v: %s", err, out.String())
		}
		return nil
	}, b.disk)
	if err != nil {
		t.Fatal(err)
	}
}

func (b localDiskBox) bootOnce(t *testing.T) {
	t.Helper()
	cmd := exec.Command(b.boot)
	cmd.Env = b.env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the boot script failed: %v: %s", err, out)
	}
}

func lines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestTheMoveCopiesTheHomeBindsItAndLinksTheStore(t *testing.T) {
	box := newLocalDiskBox(t)
	box.move(t)
	copied := filepath.Join(box.disk, ".cc-remote", "home")
	if data, err := os.ReadFile(filepath.Join(copied, ".bashrc")); err != nil || string(data) != "base" {
		t.Errorf("the local copy of the home holds %q, %v", data, err)
	}
	if got, want := lines(t, box.mounts), []string{"--bind " + copied + " " + box.home}; !slices.Equal(got, want) {
		t.Errorf("mounted %q, want %q", got, want)
	}
	if target, err := os.Readlink(box.store); err != nil || target != filepath.Join(box.disk, ".cc-remote", "store") {
		t.Errorf("the store links to %q, %v", target, err)
	}
	if info, err := os.Stat(box.store); err != nil || !info.IsDir() {
		t.Errorf("the store link does not reach a directory: %v", err)
	}
	services := lines(t, box.services)
	if len(services) != 1 || !strings.HasPrefix(services[0], "services create "+LocalDiskService+" --cmd sudo --args -n,sh,-c,"+box.boot+" && exec sleep infinity") {
		t.Errorf("services %q do not run the boot script", services)
	}
}

func TestABootBindsTheCopyItAlreadyHasAndCopiesOnlyWhenTheDiskLostIt(t *testing.T) {
	box := newLocalDiskBox(t)
	box.move(t)
	copied := filepath.Join(box.disk, ".cc-remote", "home")
	kept := filepath.Join(copied, "kept")
	if err := os.WriteFile(kept, []byte("work"), 0o600); err != nil {
		t.Fatal(err)
	}
	box.bootOnce(t)
	if got := lines(t, box.mounts); len(got) != 1 {
		t.Errorf("a boot with the home still mounted mounted again: %q", got)
	}
	if err := os.Remove(filepath.Join(filepath.Dir(box.mounts), "mounted")); err != nil {
		t.Fatal(err)
	}
	box.bootOnce(t)
	if got := lines(t, box.mounts); len(got) != 2 {
		t.Errorf("a cold boot did not mount the home again: %q", got)
	}
	if data, err := os.ReadFile(kept); err != nil || string(data) != "work" {
		t.Errorf("a cold boot replaced the local copy: %q, %v", data, err)
	}
	if err := os.RemoveAll(copied); err != nil {
		t.Fatal(err)
	}
	box.bootOnce(t)
	if data, err := os.ReadFile(filepath.Join(copied, ".bashrc")); err != nil || string(data) != "base" {
		t.Errorf("a boot after the disk lost its copy holds %q, %v", data, err)
	}
	if _, err := os.Stat(copied + ".partial"); !os.IsNotExist(err) {
		t.Errorf("the copy left its partial behind: %v", err)
	}
}
