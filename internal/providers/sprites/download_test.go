package sprites

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-remote/internal/providers"
)

type downloadRunner struct {
	*fakeSprites
	body     string
	stderr   string
	exitCode int
	reads    [][]string
}

func (r *downloadRunner) Run(ctx context.Context, cmd providers.Command) (providers.Result, error) {
	if len(cmd.Args) < 4 || !strings.Contains(cmd.Args[3], "/fs/read") {
		return r.fakeSprites.Run(ctx, cmd)
	}
	if cmd.Name != r.cli || cmd.Stdout == nil || cmd.Stdin != nil {
		r.t.Fatalf("download ran %q with stdout %v and stdin %v", cmd.Name, cmd.Stdout, cmd.Stdin)
	}
	r.reads = append(r.reads, slices.Clone(cmd.Args))
	if _, err := io.WriteString(cmd.Stdout, r.body); err != nil {
		return providers.Result{}, err
	}
	return providers.Result{Stderr: []byte(r.stderr), ExitCode: r.exitCode}, nil
}

func TestDownloadStreamsAFileThroughTheAPI(t *testing.T) {
	read := []string{"api", "-o", "acme", "/v1/sprites/s1/fs/read?path=%2Fvar%2Flib%2Fcc-remote%2Fbuild%2Fpayload.sqfs", "--", "-sS", "-f"}
	tests := []struct {
		name     string
		sprite   string
		body     string
		stderr   string
		exitCode int
		reads    [][]string
		is       error
		message  string
	}{
		{name: "streams the body", sprite: "s1", body: "hsqs payload bytes", reads: [][]string{read}},
		{
			name: "fails on an HTTP error", sprite: "s1", stderr: "curl: (22) The requested URL returned error: 404", exitCode: 22, reads: [][]string{read},
			message: "downloading /var/lib/cc-remote/build/payload.sqfs from sprite s1: <cli> api -o acme " + read[3] + " -- -sS -f: exit 22: curl: (22) The requested URL returned error: 404",
		},
		{name: "refuses a missing sprite", sprite: "gone", is: providers.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, fake := newProvider(t)
			if _, err := p.Create(t.Context(), spec("s1", nil)); err != nil {
				t.Fatal(err)
			}
			runner := &downloadRunner{fakeSprites: fake, body: tt.body, stderr: tt.stderr, exitCode: tt.exitCode}
			p.Runner = runner
			var out bytes.Buffer
			err := p.Download(t.Context(), tt.sprite, "/var/lib/cc-remote/build/payload.sqfs", &out)
			switch {
			case tt.is != nil:
				if !errors.Is(err, tt.is) {
					t.Errorf("Download = %v, want %v", err, tt.is)
				}
			case tt.message != "":
				if want := strings.Replace(tt.message, "<cli>", p.CLI, 1); err == nil || err.Error() != want {
					t.Errorf("Download = %v, want %q", err, want)
				}
			case err != nil:
				t.Errorf("Download = %v", err)
			}
			if out.String() != tt.body {
				t.Errorf("wrote %q, want %q", out.String(), tt.body)
			}
			if !slices.EqualFunc(runner.reads, tt.reads, slices.Equal) {
				t.Errorf("reads = %q, want %q", runner.reads, tt.reads)
			}
		})
	}
}
