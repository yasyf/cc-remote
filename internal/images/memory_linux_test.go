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

type memoryBox struct {
	cgroup, boot, services, created string
	env                             []string
}

func newMemoryBox(t *testing.T) memoryBox {
	t.Helper()
	dir := t.TempDir()
	box := memoryBox{
		cgroup:   filepath.Join(dir, "cgroup"),
		boot:     filepath.Join(dir, "opt", "memory.sh"),
		services: filepath.Join(dir, "services"),
		created:  filepath.Join(dir, "created"),
	}
	bin := filepath.Join(dir, "bin")
	for _, made := range []string{box.cgroup, bin} {
		if err := os.MkdirAll(made, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	box.reset(t)
	spriteEnv := "#!/bin/sh\necho \"$@\" >> " + box.services + "\ncase \"$2\" in\n" +
		"  get) [ -e " + box.created + " ] ;;\n" +
		"  create) touch " + box.created + " ;;\n" +
		"  delete) rm " + box.created + " ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(bin, "sprite-env"), []byte(spriteEnv), 0o755); err != nil {
		t.Fatal(err)
	}
	box.env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	return box
}

func (b memoryBox) reset(t *testing.T) {
	t.Helper()
	for _, file := range []string{"memory.high", "memory.max"} {
		if err := os.WriteFile(filepath.Join(b.cgroup, file), []byte("max\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func (b memoryBox) limit(t *testing.T) {
	t.Helper()
	script := strings.Replace(limitMemory, "boot="+memoryBoot+"\n", "boot="+b.boot+"\n", 1)
	script = strings.Replace(script, "cgroup=/sys/fs/cgroup\n", "cgroup="+b.cgroup+"\n", 1)
	err := Scripts{}.LimitMemory(context.Background(), func(_ context.Context, argv []string, _ io.Reader) error {
		if want := []string{"sudo", "--preserve-env=PATH", "bash", "-s", "8192", "4096"}; !slices.Equal(argv, want) {
			t.Errorf("ran %q, want %q", argv, want)
		}
		cmd := exec.Command("bash", "-s", "8192", "4096")
		cmd.Stdin, cmd.Env = strings.NewReader(script), b.env
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			t.Fatalf("the limit failed: %v: %s", err, out.String())
		}
		return nil
	}, 8192, 4096)
	if err != nil {
		t.Fatal(err)
	}
}

func (b memoryBox) limits(t *testing.T) (high, limit string) {
	t.Helper()
	read := func(file string) string {
		data, err := os.ReadFile(filepath.Join(b.cgroup, file))
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(data))
	}
	return read("memory.high"), read("memory.max")
}

func TestTheLimitCapsTheContainerAndArmsAServiceThatCapsItAgainAtBoot(t *testing.T) {
	box := newMemoryBox(t)
	box.limit(t)
	if high, limit := box.limits(t); high != "4096" || limit != "8192" {
		t.Errorf("after the limit memory.high = %s and memory.max = %s, want 4096 and 8192", high, limit)
	}
	services := lines(t, box.services)
	if len(services) != 2 || services[0] != "services get "+MemoryService || !strings.HasPrefix(services[1], "services create "+MemoryService+" --cmd sudo --args -n,sh,-c,"+box.boot+" && exec sleep infinity") {
		t.Errorf("services %q do not create the boot service once", services)
	}

	box.reset(t)
	cmd := exec.Command(box.boot)
	cmd.Env = box.env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the boot script failed: %v: %s", err, out)
	}
	if high, limit := box.limits(t); high != "4096" || limit != "8192" {
		t.Errorf("after a boot memory.high = %s and memory.max = %s, want 4096 and 8192", high, limit)
	}
}

func TestALimitOnAWokenMachineCapsItAgainAndLeavesItsBootServiceRunning(t *testing.T) {
	box := newMemoryBox(t)
	box.limit(t)
	box.reset(t)
	box.limit(t)
	if high, limit := box.limits(t); high != "4096" || limit != "8192" {
		t.Errorf("after the second limit memory.high = %s and memory.max = %s, want 4096 and 8192", high, limit)
	}
	services := lines(t, box.services)
	if len(services) != 5 || services[3] != "services delete "+MemoryService || !strings.HasPrefix(services[4], "services create "+MemoryService) {
		t.Errorf("the second limit ran %q, want the stopped boot service deleted and created running again", services[2:])
	}
}
