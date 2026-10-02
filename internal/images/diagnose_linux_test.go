package images

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

func TestDiagnoseMemoryParses(t *testing.T) {
	cmd := exec.Command("bash", "-n")
	cmd.Stdin = strings.NewReader(diagnoseMemory)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bash -n = %v\n%s", err, out)
	}
}

func TestDiagnoseMemoryReadsTheCgroupTheKernelLogAndTheProgress(t *testing.T) {
	const (
		invoked = "node invoked oom-killer: gfp_mask=0x140cca(GFP_HIGHUSER_MOVABLE|__GFP_COMP), order=0, oom_score_adj=0"
		chosen  = "oom-kill:constraint=CONSTRAINT_MEMCG,nodemask=(null),cpuset=/,mems_allowed=0,oom_memcg=/build,task_memcg=/build,task=node,pid=812,uid=1000"
		killed  = "Memory cgroup out of memory: Killed process 812 (node) total-vm:2048kB, anon-rss:1024kB, file-rss:0kB, shmem-rss:0kB, UID:1000 pgtables:64kB oom_score_adj:0"
	)
	var busy strings.Builder
	kills := make([]string, 0, 25)
	for i := range 25 {
		kill := fmt.Sprintf("Out of memory: Killed process %d (worker)", 1000+i)
		fmt.Fprintf(&busy, "3,%d,%d,-;%s\n", 200+i, 400000000+i, kill)
		kills = append(kills, kill)
	}
	lastKills, err := json.Marshal(kills[5:])
	if err != nil {
		t.Fatal(err)
	}
	progress := "home/agent/.cc-remote/verify-progress"
	tests := []struct {
		name  string
		files [][2]string
		kmsg  func(t *testing.T, path string)
		want  string
	}{
		{
			name: "a memory-accounted cgroup with kernel OOM records among other lines",
			files: [][2]string{
				{"proc/self/cgroup", "12:memory:/legacy\n0::/build\n"},
				{"sys/fs/cgroup/build/memory.current", "734003200\n"},
				{"sys/fs/cgroup/build/memory.max", "1073741824\n"},
				{"sys/fs/cgroup/build/memory.peak", "1073741824\n"},
				{"sys/fs/cgroup/build/memory.events", "low 0\nhigh 0\nmax 41\noom 2\noom_kill 1\noom_group_kill 0\n"},
				{"dev/kmsg", "6,100,1000000,-;systemd[1]: Started session.\n" +
					"4,101,343000000,-;" + invoked + "\n" +
					" SUBSYSTEM=memory\n" +
					"6,102,343000001,-;" + chosen + "\n" +
					"3,103,343000002,-,caller=T812;" + killed + "\n" +
					"6,104,343000003,-;eth0: link up\n"},
				{progress, "font:0\n"},
			},
			want: `{"cgroup":"/sys/fs/cgroup/build","memory.current":734003200,"memory.max":1073741824,"memory.peak":1073741824,` +
				`"memory.events":{"low":0,"high":0,"max":41,"oom":2,"oom_kill":1,"oom_group_kill":0},` +
				`"oom":["` + invoked + `","` + chosen + `","` + killed + `"],"progress":"font:0"}`,
		},
		{
			name: "a leaf without memory accounting reads its nearest accounted ancestor, and unreadable values stay null",
			files: [][2]string{
				{"proc/self/cgroup", "0::/build.slice/leaf\n"},
				{"sys/fs/cgroup/build.slice/leaf/cgroup.procs", "812\n"},
				{"sys/fs/cgroup/build.slice/memory.current", "1048576\n"},
				{"sys/fs/cgroup/build.slice/memory.max", "2147483648\n"},
				{"sys/fs/cgroup/build.slice/memory.peak", "12 34\n"},
				{progress, "font:0; rm -rf /\n"},
			},
			want: `{"cgroup":"/sys/fs/cgroup/build.slice","memory.current":1048576,"memory.max":2147483648,"memory.peak":null,"memory.events":null,"oom":null,"progress":null}`,
		},
		{
			name: "the root cgroup keeps the last twenty OOM records",
			files: [][2]string{
				{"proc/self/cgroup", "0::/\n"},
				{"sys/fs/cgroup/cgroup.procs", "1\n"},
				{"dev/kmsg", busy.String()},
				{progress, "plugin:hooks@market\n"},
			},
			want: `{"cgroup":"/sys/fs/cgroup","memory.current":null,"memory.max":null,"memory.peak":null,"memory.events":null,"oom":` + string(lastKills) + `,"progress":"plugin:hooks@market"}`,
		},
		{
			name: "a readable kernel log without OOM records and no progress",
			files: [][2]string{
				{"proc/self/cgroup", "0::/build\n"},
				{"sys/fs/cgroup/build/memory.current", "4096\n"},
				{"sys/fs/cgroup/build/memory.max", "8192\n"},
				{"sys/fs/cgroup/build/memory.peak", "8192\n"},
				{"sys/fs/cgroup/build/memory.events", "low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\noom_group_kill 0\n"},
				{"dev/kmsg", "6,1,1,-;eth0: link up\n"},
			},
			want: `{"cgroup":"/sys/fs/cgroup/build","memory.current":4096,"memory.max":8192,"memory.peak":8192,` +
				`"memory.events":{"low":0,"high":0,"max":0,"oom":0,"oom_kill":0,"oom_group_kill":0},"oom":[],"progress":null}`,
		},
		{
			name: "a kernel log that never ends, like the device, is read until it goes quiet",
			files: [][2]string{
				{"proc/self/cgroup", "0::/build\n"},
				{"sys/fs/cgroup/build/memory.current", "4096\n"},
			},
			kmsg: endlessKernelLog("6,1,1,-;eth0: link up\n3,2,2,-;" + killed + "\n"),
			want: `{"cgroup":"/sys/fs/cgroup/build","memory.current":4096,"memory.max":null,"memory.peak":null,"memory.events":null,"oom":["` + killed + `"],"progress":null}`,
		},
		{
			name: "a kernel log that passes the read test but cannot be read is null, not empty",
			files: [][2]string{
				{"proc/self/cgroup", "0::/build\n"},
				{"sys/fs/cgroup/build/memory.current", "4096\n"},
			},
			kmsg: unreadableKernelLog,
			want: `{"cgroup":"/sys/fs/cgroup/build","memory.current":4096,"memory.max":null,"memory.peak":null,"memory.events":null,"oom":null,"progress":null}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for _, file := range tt.files {
				writePluginTestFile(t, filepath.Join(root, file[0]), []byte(file[1]), 0o644)
			}
			if tt.kmsg != nil {
				tt.kmsg(t, filepath.Join(root, "dev/kmsg"))
			}
			capture := func(ctx context.Context, argv []string, stdin io.Reader) ([]byte, error) {
				if want := []string{"sudo", "bash", "-s"}; !slices.Equal(argv, want) {
					t.Errorf("argv = %q, want %q", argv, want)
				}
				cmd := exec.CommandContext(ctx, "bash", "-s", root, "/home/agent")
				cmd.Stdin = stdin
				var stderr bytes.Buffer
				cmd.Stderr = &stderr
				out, err := cmd.Output()
				if err != nil {
					return nil, fmt.Errorf("%w: %s", err, stderr.Bytes())
				}
				if stderr.Len() != 0 {
					t.Errorf("stderr = %q, want none", stderr.String())
				}
				return out, nil
			}
			got, err := Scripts{}.DiagnoseMemory(t.Context(), capture)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("evidence = %s\nwant       %s", got, tt.want)
			}
		})
	}
}

func endlessKernelLog(records string) func(*testing.T, string) {
	return func(t *testing.T, path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(path, 0o644); err != nil {
			t.Fatal(err)
		}
		writer, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = writer.Close() })
		if _, err := writer.WriteString(records); err != nil {
			t.Fatal(err)
		}
	}
}

func unreadableKernelLog(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}
