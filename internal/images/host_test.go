package images

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
)

const presignedURL = "https://bucket.s3.us-west-2.amazonaws.com/cache-seeds/sources/" + digest + "/tools.sqfs?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=0123456789abcdef"

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
	presigned, err := ParsePayloadURL(presignedURL)
	if err != nil {
		t.Fatal(err)
	}
	run := `exec bash "$HOME/.cc-remote/plugins.sh" "$@"`
	tests := []struct {
		name string
		op   func(context.Context, Exec) error
		want call
	}{
		{"provision packages", func(ctx context.Context, exec Exec) error { return scripts.Provision(ctx, exec, PhasePackages) }, call{[]string{"sudo", "bash", "-s", "packages"}, "provision"}},
		{"provision resident packages", func(ctx context.Context, exec Exec) error {
			return scripts.Provision(ctx, exec, PhasePackages, PackagesResident, digest)
		}, call{[]string{"sudo", "bash", "-s", "packages", "resident", digest}, "provision"}},
		{"provision loader", func(ctx context.Context, exec Exec) error { return scripts.Provision(ctx, exec, PhaseLoader) }, call{[]string{"sudo", "bash", "-s", "loader"}, "provision"}},
		{"provision tools", func(ctx context.Context, exec Exec) error { return scripts.Provision(ctx, exec, PhaseTools) }, call{[]string{"sudo", "bash", "-s", "tools"}, "provision"}},
		{"provision payload", func(ctx context.Context, exec Exec) error {
			return scripts.Provision(ctx, exec, PhasePayload, digest, fingerprint)
		}, call{[]string{"sudo", "--preserve-env=PATH", "bash", "-s", "payload", digest, fingerprint}, "provision"}},
		{"provision pack", func(ctx context.Context, exec Exec) error {
			return scripts.Provision(ctx, exec, PhasePack, fingerprint)
		}, call{[]string{"sudo", "bash", "-s", "pack", fingerprint}, "provision"}},
		{"stage payload", func(ctx context.Context, exec Exec) error {
			return scripts.StagePayload(ctx, exec, strings.NewReader("image"), digest)
		}, call{[]string{"sudo", "bash", "-c", stagePayload, "stage-payload", digest}, "image"}},
		{"fetch payload", func(ctx context.Context, exec Exec) error {
			return scripts.FetchPayload(ctx, exec, presigned, digest, 1643491328)
		}, call{[]string{"sudo", "bash", "-c", fetchPayload, "fetch-payload", digest, "1643491328"}, "url = \"" + presignedURL + "\"\n"}},
		{"stage", func(ctx context.Context, exec Exec) error { return scripts.StagePlugins(ctx, exec, "") }, call{[]string{"sh", "-c", stagePlugins}, "plugins"}},
		{"stage enabling the payload's plugins", func(ctx context.Context, exec Exec) error {
			return scripts.StagePlugins(ctx, exec, digest)
		}, call{[]string{"sh", "-c", stagePluginsEnabling, "stage-plugins", "/opt/cc-remote/payload/" + digest}, "plugins"}},
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
			for _, arg := range got.argv {
				if strings.Contains(arg, "X-Amz") {
					t.Errorf("argv %q carries the payload URL", got.argv)
				}
			}
		})
	}
}

func TestDiagnoseMemoryReturnsCompactEvidence(t *testing.T) {
	failed := errors.New("sudo on m exited 1")
	tests := []struct {
		name    string
		out     string
		err     error
		want    string
		message string
	}{
		{name: "evidence", out: "{\"cgroup\": \"/sys/fs/cgroup\",\n  \"oom\": []}\n", want: `{"cgroup":"/sys/fs/cgroup","oom":[]}`},
		{name: "a failed run", out: "{}", err: failed, message: "diagnose memory: sudo on m exited 1"},
		{name: "output that is not JSON", out: "api_key: hunter2\n", message: "diagnose memory: the evidence is not JSON: invalid character 'a' looking for beginning of value"},
		{name: "no output", message: "diagnose memory: the evidence is not JSON: unexpected end of JSON input"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var r recorder
			capture := func(ctx context.Context, argv []string, stdin io.Reader) ([]byte, error) {
				if err := r.exec(ctx, argv, stdin); err != nil {
					return nil, err
				}
				return []byte(tt.out), tt.err
			}
			got, err := Scripts{}.DiagnoseMemory(context.Background(), capture)
			if want := (call{[]string{"sudo", "bash", "-s"}, diagnoseMemory}); len(r.calls) != 1 || !slices.Equal(r.calls[0].argv, want.argv) || r.calls[0].stdin != want.stdin {
				t.Errorf("calls = %q, want one %q fed the diagnosis", r.calls, want.argv)
			}
			if tt.message != "" {
				if err == nil || err.Error() != tt.message || got != nil {
					t.Errorf("DiagnoseMemory = %s, %v, want %q", got, err, tt.message)
				}
				if tt.err != nil && !errors.Is(err, tt.err) {
					t.Errorf("DiagnoseMemory = %v, want it to wrap %v", err, tt.err)
				}
				return
			}
			if err != nil || string(got) != tt.want {
				t.Errorf("DiagnoseMemory = %s, %v, want %s", got, err, tt.want)
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
		{"fetch payload digest", func(ctx context.Context, exec Exec) error {
			return scripts.FetchPayload(ctx, exec, PayloadURL{}, "../"+digest, 1)
		}, `fetch payload: "../` + digest + `" is not a sha256 digest`},
		{"fetch payload size", func(ctx context.Context, exec Exec) error {
			return scripts.FetchPayload(ctx, exec, PayloadURL{}, digest, 0)
		}, "fetch payload: size 0 is not a byte count"},
		{"stage plugins payload digest", func(ctx context.Context, exec Exec) error {
			return scripts.StagePlugins(ctx, exec, "../"+digest)
		}, `stage plugins.sh: payload "../` + digest + `" is not a sha256 digest`},
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

func TestParsePayloadURLKeepsTheURLOutOfEveryRendering(t *testing.T) {
	ascii := "payload url: the URL must be printable ASCII without spaces, quotes or backslashes"
	shape := "payload url: the URL must be an absolute https URL with a host and no userinfo"
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"a presigned url", presignedURL, ""},
		{"http", "http://bucket.example/tools.sqfs", shape},
		{"file", "file:///etc/passwd", shape},
		{"nothing", "", "payload url: the command printed nothing"},
		{"no host", "https:///tools.sqfs", shape},
		{"userinfo", "https://user:secret@bucket.example/tools.sqfs", shape},
		{"a space", "https://bucket.example/tools sqfs", ascii},
		{"a double quote", `https://bucket.example/tools.sqfs"`, ascii},
		{"a backslash", `https://bucket.example/tools\sqfs`, ascii},
		{"a newline", "https://bucket.example/tools.sqfs\n", ascii},
		{"a carriage return", "https://bucket.example/tools.sqfs\r", ascii},
		{"a second line", "https://bucket.example/tools.sqfs\nhttps://bucket.example/other.sqfs", ascii},
		{"a delete byte", "https://bucket.example/tools.sqfs\x7f", ascii},
		{"over the limit", "https://bucket.example/" + strings.Repeat("a", payloadURLLimit), "payload url: 8215 bytes is over the 8192-byte limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source, err := ParsePayloadURL(tt.raw)
			if tt.want != "" {
				if err == nil || err.Error() != tt.want {
					t.Fatalf("ParsePayloadURL = %v, want %q", err, tt.want)
				}
				if strings.Contains(err.Error(), "bucket") {
					t.Errorf("the error %q echoes the input", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := string(source.curlConfig()); got != "url = \""+tt.raw+"\"\n" {
				t.Errorf("curlConfig = %q", got)
			}
			var text, structured bytes.Buffer
			slog.New(slog.NewTextHandler(&text, nil)).Info("fetching", "url", source)
			slog.New(slog.NewJSONHandler(&structured, nil)).Info("fetching", "url", source)
			renderings := map[string]string{
				"fmt":  fmt.Sprintf("%v %+v %#v %s %q %d %x", source, source, source, source, source, source, source),
				"text": text.String(),
				"json": structured.String(),
			}
			for name, rendered := range renderings {
				if strings.Contains(rendered, "X-Amz") || strings.Contains(rendered, "bucket") || strings.Contains(rendered, digest) {
					t.Errorf("the %s rendering %q carries the URL", name, rendered)
				}
			}
			if want := strings.TrimSuffix(strings.Repeat("[payload url] ", 7), " "); renderings["fmt"] != want {
				t.Errorf("fmt rendering = %q, want %q", renderings["fmt"], want)
			}
		})
	}
}
