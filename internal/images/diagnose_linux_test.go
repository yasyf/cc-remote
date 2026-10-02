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

func TestDiagnoseMemoryReadsTheCgroupsTheBootTheKernelLogAndTheProgress(t *testing.T) {
	const (
		invoked      = "node invoked oom-killer: gfp_mask=0x140cca(GFP_HIGHUSER_MOVABLE|__GFP_COMP), order=0, oom_score_adj=0"
		chosen       = "oom-kill:constraint=CONSTRAINT_MEMCG,nodemask=(null),cpuset=/,mems_allowed=0,oom_memcg=/build,task_memcg=/build,task=node,pid=812,uid=1000"
		killed       = "Memory cgroup out of memory: Killed process 812 (node) total-vm:2048kB, anon-rss:1024kB, file-rss:0kB, shmem-rss:0kB, UID:1000 pgtables:64kB oom_score_adj:0"
		then         = "0b1c2d3e-4f50-4a6b-8c7d-9e0f1a2b3c4d"
		now          = "5f6e7d8c-9b0a-4c1d-8e2f-3a4b5c6d7e8f"
		progress     = "home/agent/.cc-remote/verify-progress"
		recorded     = "home/agent/.cc-remote/verify-context"
		bootID       = "proc/sys/kernel/random/boot_id"
		vmstat       = "proc/vmstat"
		rootReading  = `{"cgroup":"/sys/fs/cgroup","memory.current":null,"memory.max":null,"memory.peak":null,"memory.events":null}`
		buildReading = `{"cgroup":"/sys/fs/cgroup/build","memory.current":734003200,"memory.max":1073741824,"memory.peak":1073741824,` +
			`"memory.events":{"low":0,"high":0,"max":41,"oom":2,"oom_kill":1,"oom_group_kill":0}}`
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
	build := [][2]string{
		{"sys/fs/cgroup/build/memory.current", "734003200\n"},
		{"sys/fs/cgroup/build/memory.max", "1073741824\n"},
		{"sys/fs/cgroup/build/memory.peak", "1073741824\n"},
		{"sys/fs/cgroup/build/memory.events", "low 0\nhigh 0\nmax 41\noom 2\noom_kill 1\noom_group_kill 0\n"},
	}
	tests := []struct {
		name  string
		files [][2]string
		links [][2]string
		kmsg  func(t *testing.T, path string)
		want  string
	}{
		{
			name: "the failed exec's cgroup on the same boot is read beside the diagnostic's own, with kernel OOM records among other lines",
			files: slices.Concat(build, [][2]string{
				{"proc/self/cgroup", "12:memory:/legacy\n0::/diagnose\n"},
				{"sys/fs/cgroup/diagnose/memory.current", "4096\n"},
				{"sys/fs/cgroup/diagnose/memory.max", "max\n"},
				{"sys/fs/cgroup/diagnose/memory.peak", "8192\n"},
				{"sys/fs/cgroup/diagnose/memory.events", "low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\noom_group_kill 0\n"},
				{bootID, then + "\n"},
				{recorded, "boot_id " + then + "\ncgroup /build\n"},
				{vmstat, "nr_free_pages 1024\noom_kill 1\npgfault 99\n"},
				{"dev/kmsg", "6,100,1000000,-;systemd[1]: Started session.\n" +
					"4,101,343000000,-;" + invoked + "\n" +
					" SUBSYSTEM=memory\n" +
					"6,102,343000001,-;" + chosen + "\n" +
					"3,103,343000002,-,caller=T812;" + killed + "\n" +
					"6,104,343000003,-;eth0: link up\n"},
				{progress, "font:0\n"},
			}),
			want: `{"bootId":{"then":"` + then + `","now":"` + then + `"},"sameBoot":true,"progress":"font:0","failedExecCgroup":"/sys/fs/cgroup/build",` +
				`"cgroups":{"failedExec":` + buildReading + `,"diagnostic":{"cgroup":"/sys/fs/cgroup/diagnose","memory.current":4096,"memory.max":"max","memory.peak":8192,` +
				`"memory.events":{"low":0,"high":0,"max":0,"oom":0,"oom_kill":0,"oom_group_kill":0}}},` +
				`"vmstat.oom_kill":1,"oom":["` + invoked + `","` + chosen + `","` + killed + `"]}`,
		},
		{
			name: "a changed boot keeps both boot IDs and reports the failed exec's vanished cgroup as null, not its parent",
			files: [][2]string{
				{"proc/self/cgroup", "0::/\n"},
				{"sys/fs/cgroup/cgroup.procs", "1\n"},
				{"sys/fs/cgroup/sprite/memory.current", "999\n"},
				{bootID, now + "\n"},
				{recorded, "boot_id " + then + "\ncgroup /sprite/exec-7\n"},
				{vmstat, "oom_kill 0\n"},
				{"dev/kmsg", "6,1,1,-;eth0: link up\n"},
				{progress, "plugins\n"},
			},
			want: `{"bootId":{"then":"` + then + `","now":"` + now + `"},"sameBoot":false,"progress":"plugins","failedExecCgroup":"/sys/fs/cgroup/sprite/exec-7",` +
				`"cgroups":{"failedExec":null,"diagnostic":` + rootReading + `},"vmstat.oom_kill":0,"oom":[]}`,
		},
		{
			name: "a failed exec in the root cgroup is read there, where memory is not accounted",
			files: [][2]string{
				{"proc/self/cgroup", "0::/\n"},
				{"sys/fs/cgroup/cgroup.procs", "1\n"},
				{bootID, then + "\n"},
				{recorded, "boot_id " + then + "\ncgroup /\n"},
				{vmstat, "oom_kill 0\n"},
			},
			want: `{"bootId":{"then":"` + then + `","now":"` + then + `"},"sameBoot":true,"progress":null,"failedExecCgroup":"/sys/fs/cgroup",` +
				`"cgroups":{"failedExec":` + rootReading + `,"diagnostic":` + rootReading + `},"vmstat.oom_kill":0,"oom":null}`,
		},
		{
			name: "an unreadable current boot leaves sameBoot unknown and still reads the recorded cgroup",
			files: slices.Concat(build, [][2]string{
				{"proc/self/cgroup", "0::/\n"},
				{recorded, "boot_id " + then + "\ncgroup /build\n"},
			}),
			want: `{"bootId":{"then":"` + then + `","now":null},"sameBoot":null,"progress":null,"failedExecCgroup":"/sys/fs/cgroup/build",` +
				`"cgroups":{"failedExec":` + buildReading + `,"diagnostic":` + rootReading + `},"vmstat.oom_kill":null,"oom":null}`,
		},
		{
			name: "without a recorded context the old boot and the failed exec stay unknown, and vmstat without oom_kill is null",
			files: slices.Concat(build, [][2]string{
				{"proc/self/cgroup", "0::/\n"},
				{bootID, now + "\n"},
				{vmstat, "nr_free_pages 1024\npgfault 99\n"},
			}),
			want: `{"bootId":{"then":null,"now":"` + now + `"},"sameBoot":null,"progress":null,"failedExecCgroup":null,` +
				`"cgroups":{"failedExec":null,"diagnostic":` + rootReading + `},"vmstat.oom_kill":null,"oom":null}`,
		},
		{
			name: "a boot ID with trailing text and a cgroup climbing out of the hierarchy are rejected",
			files: [][2]string{
				{"proc/self/cgroup", "0::/\n"},
				{"etc/memory.current", "1\n"},
				{bootID, then + "\n"},
				{recorded, "boot_id " + then + "; rm -rf /\ncgroup /../../../etc\n"},
			},
			want: `{"bootId":{"then":null,"now":"` + then + `"},"sameBoot":null,"progress":null,"failedExecCgroup":null,` +
				`"cgroups":{"failedExec":null,"diagnostic":` + rootReading + `},"vmstat.oom_kill":null,"oom":null}`,
		},
		{
			name: "an uppercase boot ID and a parent-directory component are rejected",
			files: slices.Concat(build, [][2]string{
				{"proc/self/cgroup", "0::/\n"},
				{bootID, then + "\n"},
				{recorded, "boot_id " + strings.ToUpper(then) + "\ncgroup /build/..\n"},
			}),
			want: `{"bootId":{"then":null,"now":"` + then + `"},"sameBoot":null,"progress":null,"failedExecCgroup":null,` +
				`"cgroups":{"failedExec":null,"diagnostic":` + rootReading + `},"vmstat.oom_kill":null,"oom":null}`,
		},
		{
			name: "repeated context lines are rejected rather than chosen between",
			files: slices.Concat(build, [][2]string{
				{"proc/self/cgroup", "0::/\n"},
				{bootID, then + "\n"},
				{recorded, "boot_id " + then + "\ncgroup /build\nboot_id " + now + "\ncgroup /\n"},
			}),
			want: `{"bootId":{"then":null,"now":"` + then + `"},"sameBoot":null,"progress":null,"failedExecCgroup":null,` +
				`"cgroups":{"failedExec":null,"diagnostic":` + rootReading + `},"vmstat.oom_kill":null,"oom":null}`,
		},
		{
			name: "a recorded cgroup that resolves outside the hierarchy is rejected, not read",
			files: [][2]string{
				{"proc/self/cgroup", "0::/\n"},
				{"outside/memory.current", "1\n"},
				{bootID, then + "\n"},
				{recorded, "boot_id " + then + "\ncgroup /escape\n"},
			},
			links: [][2]string{{"sys/fs/cgroup/escape", "outside"}},
			want: `{"bootId":{"then":"` + then + `","now":"` + then + `"},"sameBoot":true,"progress":null,"failedExecCgroup":null,` +
				`"cgroups":{"failedExec":null,"diagnostic":` + rootReading + `},"vmstat.oom_kill":null,"oom":null}`,
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
			want: `{"bootId":{"then":null,"now":null},"sameBoot":null,"progress":null,"failedExecCgroup":null,` +
				`"cgroups":{"failedExec":null,"diagnostic":{"cgroup":"/sys/fs/cgroup/build.slice","memory.current":1048576,"memory.max":2147483648,"memory.peak":null,"memory.events":null}},` +
				`"vmstat.oom_kill":null,"oom":null}`,
		},
		{
			name: "the root cgroup keeps the last twenty OOM records",
			files: [][2]string{
				{"proc/self/cgroup", "0::/\n"},
				{"sys/fs/cgroup/cgroup.procs", "1\n"},
				{"dev/kmsg", busy.String()},
				{progress, "plugin:hooks@market\n"},
			},
			want: `{"bootId":{"then":null,"now":null},"sameBoot":null,"progress":"plugin:hooks@market","failedExecCgroup":null,` +
				`"cgroups":{"failedExec":null,"diagnostic":` + rootReading + `},"vmstat.oom_kill":null,"oom":` + string(lastKills) + `}`,
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
			want: `{"bootId":{"then":null,"now":null},"sameBoot":null,"progress":null,"failedExecCgroup":null,` +
				`"cgroups":{"failedExec":null,"diagnostic":{"cgroup":"/sys/fs/cgroup/build","memory.current":4096,"memory.max":8192,"memory.peak":8192,` +
				`"memory.events":{"low":0,"high":0,"max":0,"oom":0,"oom_kill":0,"oom_group_kill":0}}},"vmstat.oom_kill":null,"oom":[]}`,
		},
		{
			name: "a kernel log that never ends, like the device, is read until it goes quiet",
			files: [][2]string{
				{"proc/self/cgroup", "0::/build\n"},
				{"sys/fs/cgroup/build/memory.current", "4096\n"},
			},
			kmsg: endlessKernelLog("6,1,1,-;eth0: link up\n3,2,2,-;" + killed + "\n"),
			want: `{"bootId":{"then":null,"now":null},"sameBoot":null,"progress":null,"failedExecCgroup":null,` +
				`"cgroups":{"failedExec":null,"diagnostic":{"cgroup":"/sys/fs/cgroup/build","memory.current":4096,"memory.max":null,"memory.peak":null,"memory.events":null}},` +
				`"vmstat.oom_kill":null,"oom":["` + killed + `"]}`,
		},
		{
			name: "a kernel log that passes the read test but cannot be read is null, not empty",
			files: [][2]string{
				{"proc/self/cgroup", "0::/build\n"},
				{"sys/fs/cgroup/build/memory.current", "4096\n"},
			},
			kmsg: unreadableKernelLog,
			want: `{"bootId":{"then":null,"now":null},"sameBoot":null,"progress":null,"failedExecCgroup":null,` +
				`"cgroups":{"failedExec":null,"diagnostic":{"cgroup":"/sys/fs/cgroup/build","memory.current":4096,"memory.max":null,"memory.peak":null,"memory.events":null}},` +
				`"vmstat.oom_kill":null,"oom":null}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for _, file := range tt.files {
				writePluginTestFile(t, filepath.Join(root, file[0]), []byte(file[1]), 0o644)
			}
			for _, link := range tt.links {
				path := filepath.Join(root, link[0])
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(root, link[1]), path); err != nil {
					t.Fatal(err)
				}
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
