package namespace

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yasyf/cc-remote/internal/providers"
)

type fakeDevbox struct {
	createdAt time.Time
	stopped   bool
}

type fakeNamespace struct {
	t             *testing.T
	cli           string
	sshDir        string
	loggedIn      bool
	sshFails      providers.Result
	takenAtCreate bool

	mu     sync.Mutex
	boxes  map[string]*fakeDevbox
	calls  [][]string
	sshRun int
}

func newFakeNamespace(t *testing.T, cli, sshDir string) *fakeNamespace {
	return &fakeNamespace{t: t, cli: cli, sshDir: sshDir, loggedIn: true, boxes: map[string]*fakeDevbox{}}
}

func (f *fakeNamespace) Run(ctx context.Context, cmd providers.Command) (providers.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch cmd.Name {
	case "ssh":
		return f.ssh(ctx, cmd)
	case f.cli:
	default:
		return providers.Result{}, fmt.Errorf("%s: %w", cmd.Name, exec.ErrNotFound)
	}
	f.calls = append(f.calls, slices.Clone(cmd.Args))
	args := cmd.Args
	switch {
	case slices.Equal(args, []string{"auth", "check-login"}):
		if !f.loggedIn {
			return providers.Result{Stderr: []byte("Error: not logged in"), ExitCode: 1}, nil
		}
		return providers.Result{}, nil
	case slices.Equal(args, []string{"list", "-o", "json"}):
		return f.list(), nil
	case args[0] == "create":
		name := flag(args, "--name")
		if f.takenAtCreate {
			f.boxes[name] = &fakeDevbox{createdAt: time.Now().UTC()}
		}
		if _, ok := f.boxes[name]; ok {
			return providers.Result{Stderr: []byte("rpc error: code = AlreadyExists desc = devbox exists"), ExitCode: 1}, nil
		}
		f.boxes[name] = &fakeDevbox{createdAt: time.Date(2026, 9, 30, 9, 28, len(f.boxes), 231641000, time.UTC)}
		return providers.Result{}, nil
	case len(args) == 2 && args[0] == "configure-ssh":
		return f.configureSSH(args[1]), nil
	case len(args) == 3 && args[0] == "shutdown" && args[2] == "--force":
		f.boxes[args[1]].stopped = true
		return providers.Result{}, nil
	case len(args) == 3 && args[0] == "expire" && args[2] == "--force":
		delete(f.boxes, args[1])
		return providers.Result{}, nil
	}
	f.t.Fatalf("unexpected devbox %q", args)
	return providers.Result{}, nil
}

func flag(args []string, name string) string {
	i := slices.Index(args, name)
	if i < 0 || i+1 >= len(args) {
		return ""
	}
	return args[i+1]
}

func (f *fakeNamespace) list() providers.Result {
	if len(f.boxes) == 0 {
		return providers.Result{Stdout: []byte("No devbox available yet. Create one with `devbox create`.\n")}
	}
	var boxes []map[string]any
	for _, name := range slices.Sorted(maps.Keys(f.boxes)) {
		box := f.boxes[name]
		boxes = append(boxes, map[string]any{
			"id":                           "id-" + name,
			"name":                         name,
			"created_at":                   box.createdAt.Format(time.RFC3339Nano),
			"last_used_at":                 box.createdAt.Format(time.RFC3339Nano),
			"image_ref":                    "nscr.io/tenant/cc-remote-linux@sha256:0000",
			"resolved_image":               "nscr.io/tenant/cc-remote-linux@sha256:0000",
			"instance_shape":               map[string]any{"virtual_cpu": 8, "memory_megabytes": 16384, "machine_arch": "amd64", "os": "linux"},
			"site":                         "us-east",
			"volume_name":                  "vol-" + name,
			"volume_size_gb":               "125",
			"workspace_dir":                "/workspaces",
			"default_dir":                  "/workspaces",
			"main_user":                    "dev",
			"creator":                      "someone",
			"repository":                   "",
			"access_mode":                  "USER_PRIVATE",
			"http_access_mode":             "USER_PRIVATE",
			"busy_ensure_minimum_duration": "1800s",
			"spec":                         map[string]any{"remote_user": "dev", "workspace_dir": "/workspaces"},
		})
	}
	raw, err := json.MarshalIndent(boxes, "", "  ")
	if err != nil {
		f.t.Fatal(err)
	}
	return providers.Result{Stdout: append(raw, '\n')}
}

func (f *fakeNamespace) proxy() string { return filepath.Join(f.sshDir, "devbox-ssh-proxy") }

func (f *fakeNamespace) configureSSH(name string) providers.Result {
	if _, ok := f.boxes[name]; !ok {
		return providers.Result{Stderr: []byte("rpc error: code = NotFound desc = no such devbox"), ExitCode: 1}
	}
	alias := Alias(name)
	config := strings.Join([]string{
		"Host " + alias,
		"  ForwardAgent yes",
		"  IdentityFile " + filepath.Join(f.sshDir, alias+".key"),
		"  IdentitiesOnly yes",
		"  ProxyCommand " + f.proxy() + " ssh-proxy " + name,
		"  StrictHostKeyChecking no",
		"  UserKnownHostsFile /dev/null",
		"  User dev",
	}, "\n") + "\n"
	if err := os.MkdirAll(f.sshDir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.sshDir, alias+".ssh"), []byte(config), 0o600); err != nil {
		f.t.Fatal(err)
	}
	return providers.Result{}
}

func (f *fakeNamespace) ssh(ctx context.Context, cmd providers.Command) (providers.Result, error) {
	f.sshRun++
	args := cmd.Args
	separator := slices.Index(args, "--")
	if separator < 2 || separator != len(args)-2 || args[0] != "-T" {
		f.t.Fatalf("ssh %q", args)
	}
	host, script := args[separator-1], args[separator+1]
	name, ok := strings.CutSuffix(host, ".devbox.namespace")
	box, exists := f.boxes[name]
	if !ok || !exists {
		f.t.Fatalf("ssh to %s, which is no devbox", host)
	}
	options := map[string]string{}
	for i := 1; i < separator-1; i += 2 {
		if args[i] != "-o" {
			f.t.Fatalf("ssh %q", args)
		}
		key, value, _ := strings.Cut(args[i+1], "=")
		options[key] = value
	}
	if want := f.proxy() + " ssh-proxy " + name; options["ProxyCommand"] != want || options["StrictHostKeyChecking"] != "no" || options["BatchMode"] != "yes" {
		f.t.Fatalf("ssh options %v, want ProxyCommand %q with no host key checking in batch mode", options, want)
	}
	if f.sshFails.ExitCode != 0 {
		return f.sshFails, nil
	}
	box.stopped = false
	return providers.OSRunner{}.Run(ctx, providers.Command{Name: "sh", Args: []string{"-c", script}, Stdin: cmd.Stdin})
}

func (f *fakeNamespace) call(verb string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, call := range f.calls {
		if call[0] == verb {
			return call
		}
	}
	return nil
}
