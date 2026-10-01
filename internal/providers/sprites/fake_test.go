package sprites

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yasyf/cc-remote/internal/providers"
)

const fakeHostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFakeSpriteHostKey root@sprite"

type fakeSprite struct {
	status     string
	createdAt  time.Time
	authorized []string
}

type fakeSprites struct {
	t        *testing.T
	cli      string
	org      string
	pageSize int
	status   int

	mu      sync.Mutex
	sprites map[string]*fakeSprite
	calls   [][]string
}

func newFakeSprites(t *testing.T, cli, org string) *fakeSprites {
	return &fakeSprites{t: t, cli: cli, org: org, pageSize: 2, sprites: map[string]*fakeSprite{}}
}

func (f *fakeSprites) Run(ctx context.Context, cmd providers.Command) (providers.Result, error) {
	switch cmd.Name {
	case "ssh-keygen":
		return providers.OSRunner{}.Run(ctx, cmd)
	case f.cli:
	default:
		return providers.Result{}, fmt.Errorf("%s: %w", cmd.Name, exec.ErrNotFound)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, slices.Clone(cmd.Args))
	verb, args := cmd.Args[0], cmd.Args[1:]
	if len(args) < 2 || args[0] != "-o" || args[1] != f.org {
		f.t.Fatalf("sprite %s without -o %s: %q", verb, f.org, cmd.Args)
	}
	args = args[2:]
	switch verb {
	case "api":
		return f.api(args)
	case "create":
		if len(args) != 2 || args[0] != "--skip-console" {
			f.t.Fatalf("sprite create %q", args)
		}
		if _, ok := f.sprites[args[1]]; ok {
			return providers.Result{Stderr: []byte("sprite already exists"), ExitCode: 1}, nil
		}
		f.sprites[args[1]] = &fakeSprite{status: "running", createdAt: time.Date(2026, 9, 30, 12, 0, len(f.sprites), 0, time.UTC)}
		return providers.Result{}, nil
	case "destroy":
		if len(args) != 3 || args[0] != "-s" || args[2] != "--force" {
			f.t.Fatalf("sprite destroy %q", args)
		}
		delete(f.sprites, args[1])
		return providers.Result{}, nil
	case "exec":
		return f.exec(ctx, args, cmd.Stdin)
	}
	f.t.Fatalf("unexpected sprite %s %q", verb, args)
	return providers.Result{}, nil
}

func (f *fakeSprites) api(args []string) (providers.Result, error) {
	if len(args) != 5 || args[1] != "--" || args[2] != "-sS" || args[3] != "-w" || args[4] != "\n%{http_code}" {
		f.t.Fatalf("sprite api %q", args)
	}
	respond := func(status int, body any) (providers.Result, error) {
		raw, err := json.Marshal(body)
		if err != nil {
			f.t.Fatal(err)
		}
		return providers.Result{Stdout: fmt.Appendf(raw, "\n%d", status)}, nil
	}
	if f.status != 0 {
		return respond(f.status, map[string]string{"error": "unauthorized"})
	}
	path, err := url.Parse(args[0])
	if err != nil {
		f.t.Fatal(err)
	}
	if name, ok := strings.CutPrefix(path.Path, "/v1/sprites/"); ok {
		sprite, found := f.sprites[name]
		if !found {
			return respond(404, map[string]string{"error": "sprite not found"})
		}
		return respond(200, f.resource(name, sprite))
	}
	if path.Path != "/v1/sprites" {
		f.t.Fatalf("sprite api %s", args[0])
	}
	names := slices.Sorted(maps.Keys(f.sprites))
	start := 0
	if token := path.Query().Get("continuation_token"); token != "" {
		start, _ = strconv.Atoi(token)
	}
	limit := min(f.pageSize, len(names)-start)
	if requested, _ := strconv.Atoi(path.Query().Get("max_results")); requested < limit {
		limit = requested
	}
	page := []map[string]any{}
	for _, name := range names[start : start+limit] {
		page = append(page, f.resource(name, f.sprites[name]))
	}
	next := start + limit
	body := map[string]any{
		"data": page, "sprites": page, "name": f.org, "running": 0, "cold": 0, "warm": 0,
		"has_more": next < len(names), "next_continuation_token": nil,
		"org": map[string]any{"name": f.org, "running_limit": 10, "warm_limit": 10},
	}
	if next < len(names) {
		body["next_continuation_token"] = strconv.Itoa(next)
	}
	return respond(200, body)
}

func (f *fakeSprites) resource(name string, sprite *fakeSprite) map[string]any {
	return map[string]any{
		"id":                  "sprite-" + name,
		"name":                name,
		"status":              sprite.status,
		"version":             "0.0.2-beta.3",
		"url":                 "https://" + name + ".sprites.app",
		"url_settings":        map[string]string{"auth": "sprite", "private_access": "admins"},
		"created_at":          sprite.createdAt.Format(time.RFC3339Nano),
		"updated_at":          sprite.createdAt.Format(time.RFC3339Nano),
		"organization":        f.org,
		"last_running_at":     nil,
		"last_warming_at":     nil,
		"environment_version": nil,
	}
}

func (f *fakeSprites) exec(ctx context.Context, args []string, stdin io.Reader) (providers.Result, error) {
	if len(args) < 4 || args[0] != "-s" || args[2] != "--no-port-forward" {
		f.t.Fatalf("sprite exec %q", args)
	}
	sprite, ok := f.sprites[args[1]]
	if !ok {
		return providers.Result{Stderr: []byte("sprite not found"), ExitCode: 1}, nil
	}
	rest := args[3:]
	if rest[0] == "--no-stdin" {
		if stdin != nil {
			f.t.Fatalf("sprite exec --no-stdin with a stdin")
		}
		rest = rest[1:]
	}
	if rest[0] != "--" {
		f.t.Fatalf("sprite exec without --: %q", args)
	}
	sprite.status = "running"
	cmd := rest[1:]
	if slices.Equal(cmd, []string{"sh", "-c", AuthorizeScript}) {
		key, err := io.ReadAll(stdin)
		if err != nil {
			f.t.Fatal(err)
		}
		sprite.authorized = append(sprite.authorized, strings.TrimSpace(string(key)))
		return providers.Result{Stdout: []byte(fakeHostKey + "\n")}, nil
	}
	return providers.OSRunner{}.Run(ctx, providers.Command{Name: cmd[0], Args: cmd[1:], Stdin: stdin})
}

func (f *fakeSprites) verbs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	verbs := make([]string, 0, len(f.calls))
	for _, call := range f.calls {
		verbs = append(verbs, call[0])
	}
	return verbs
}
