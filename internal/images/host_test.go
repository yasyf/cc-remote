package images

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"
)

type call struct {
	argv  []string
	stdin string
}

type recorder struct{ calls []call }

func (r *recorder) exec(_ context.Context, argv []string, stdin io.Reader) error {
	var input []byte
	if stdin != nil {
		var err error
		if input, err = io.ReadAll(stdin); err != nil {
			return err
		}
	}
	r.calls = append(r.calls, call{argv: argv, stdin: string(input)})
	return nil
}

func TestHostOperations(t *testing.T) {
	scripts := Scripts{ProvisionScript: []byte("provision"), Plugins: []byte("plugins"), Env: []string{"PORT", "URL"}}
	fingerprint := scripts.Fingerprint()
	run := `exec bash "$HOME/.cc-remote/plugins.sh" "$@"`
	tests := []struct {
		name string
		op   func(context.Context, Exec) error
		want call
	}{
		{"provision packages", func(ctx context.Context, exec Exec) error { return scripts.Provision(ctx, exec, PhasePackages) }, call{[]string{"sudo", "bash", "-s", "packages"}, "provision"}},
		{"provision tools", func(ctx context.Context, exec Exec) error { return scripts.Provision(ctx, exec, PhaseTools) }, call{[]string{"sudo", "bash", "-s", "tools"}, "provision"}},
		{"provision payload", func(ctx context.Context, exec Exec) error {
			return scripts.Provision(ctx, exec, PhasePayload, digest, fingerprint)
		}, call{[]string{"sudo", "--preserve-env=PATH", "bash", "-s", "payload", digest, fingerprint}, "provision"}},
		{"provision pack", func(ctx context.Context, exec Exec) error {
			return scripts.Provision(ctx, exec, PhasePack, fingerprint)
		}, call{[]string{"sudo", "bash", "-s", "pack", fingerprint}, "provision"}},
		{"stage payload", func(ctx context.Context, exec Exec) error {
			return scripts.StagePayload(ctx, exec, strings.NewReader("image"), digest)
		}, call{[]string{"sudo", "sh", "-c", "install -d -m 0755 /var/lib/cc-remote/payload && cat > /var/lib/cc-remote/payload/$1.sqfs.partial", "stage-payload", digest}, "image"}},
		{"stage", scripts.StagePlugins, call{[]string{"sh", "-c", stagePlugins}, "plugins"}},
		{"install", func(ctx context.Context, exec Exec) error { return scripts.Install(ctx, exec, "token", "") }, call{[]string{"env", "bash", "-c", run, "plugins.sh", "install"}, "token\n"}},
		{"install without token", func(ctx context.Context, exec Exec) error { return scripts.Install(ctx, exec, "", "") }, call{[]string{"env", "bash", "-c", run, "plugins.sh", "install"}, "\n"}},
		{"install from payload", func(ctx context.Context, exec Exec) error { return scripts.Install(ctx, exec, "token", digest) }, call{[]string{"env", "bash", "-c", run, "plugins.sh", "install", "/opt/cc-remote/payload/" + digest}, "token\n"}},
		{"publish", func(ctx context.Context, exec Exec) error { return scripts.Publish(ctx, exec, digest) }, call{[]string{"env", "bash", "-c", run, "plugins.sh", "publish", digest}, ""}},
		{"ready", func(ctx context.Context, exec Exec) error { return scripts.Ready(ctx, exec, digest) }, call{[]string{"env", "bash", "-c", run, "plugins.sh", "ready", digest}, ""}},
		{"configure", func(ctx context.Context, exec Exec) error {
			return scripts.Configure(ctx, exec, map[string]string{"URL": "http://x y", "PORT": "8123"})
		}, call{[]string{"env", "PORT=8123", "URL=http://x y", "bash", "-c", run, "plugins.sh", "configure"}, ""}},
		{"verify", scripts.Verify, call{[]string{"env", "bash", "-c", run, "plugins.sh", "verify"}, ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var r recorder
			if err := tt.op(context.Background(), r.exec); err != nil {
				t.Fatalf("op: %v", err)
			}
			if len(r.calls) != 1 {
				t.Fatalf("calls = %d, want 1", len(r.calls))
			}
			got := r.calls[0]
			if !slices.Equal(got.argv, tt.want.argv) || got.stdin != tt.want.stdin {
				t.Errorf("call = %q stdin %q, want %q stdin %q", got.argv, got.stdin, tt.want.argv, tt.want.stdin)
			}
		})
	}
}

func TestHostOperationsRejectBadInput(t *testing.T) {
	scripts := Scripts{Env: []string{"PORT"}}
	tests := []struct {
		name string
		op   func(context.Context, Exec) error
		want string
	}{
		{"stage payload digest", func(ctx context.Context, exec Exec) error {
			return scripts.StagePayload(ctx, exec, strings.NewReader("image"), "../"+digest)
		}, `stage payload: "../` + digest + `" is not a sha256 digest`},
		{"stage payload uppercase digest", func(ctx context.Context, exec Exec) error {
			return scripts.StagePayload(ctx, exec, strings.NewReader("image"), strings.Repeat("A", 64))
		}, `stage payload: "` + strings.Repeat("A", 64) + `" is not a sha256 digest`},
		{"multi-line token", func(ctx context.Context, exec Exec) error { return scripts.Install(ctx, exec, "a\nb", digest) }, "plugins install: the GitHub token spans lines"},
		{"carriage-return token", func(ctx context.Context, exec Exec) error { return scripts.Install(ctx, exec, "a\rb", "") }, "plugins install: the GitHub token spans lines"},
		{"install payload", func(ctx context.Context, exec Exec) error { return scripts.Install(ctx, exec, "", "latest") }, `plugins install: payload "latest" is not a sha256 digest`},
		{"install payload path", func(ctx context.Context, exec Exec) error {
			return scripts.Install(ctx, exec, "", "/opt/cc-remote/payload/"+digest)
		}, `plugins install: payload "/opt/cc-remote/payload/` + digest + `" is not a sha256 digest`},
		{"publish stamp", func(ctx context.Context, exec Exec) error { return scripts.Publish(ctx, exec, "latest") }, `plugins publish: stamp "latest" is not a fingerprint`},
		{"ready stamp", func(ctx context.Context, exec Exec) error { return scripts.Ready(ctx, exec, "") }, `plugins ready: stamp "" is not a fingerprint`},
		{"missing env", func(ctx context.Context, exec Exec) error { return scripts.Configure(ctx, exec, nil) }, "plugins configure: got environment [], the inventory declares [PORT]"},
		{"extra env", func(ctx context.Context, exec Exec) error {
			return scripts.Configure(ctx, exec, map[string]string{"PORT": "1", "HOST": "x"})
		}, "plugins configure: got environment [HOST PORT], the inventory declares [PORT]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var r recorder
			err := tt.op(context.Background(), r.exec)
			if err == nil || err.Error() != tt.want {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
			if len(r.calls) != 0 {
				t.Errorf("ran %q despite the error", r.calls[0].argv)
			}
		})
	}
}
