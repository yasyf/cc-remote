package workspace

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yasyf/cc-remote/internal/frontends/orca"
)

const (
	wordnetArchive = "synthetic wordnet archive bytes"
	keySentinel    = "sk-synthetic-wordnet-sentinel"

	stubFixture = `import sqlite3
import sys
from pathlib import Path


def build(database, mode):
    database.parent.mkdir(parents=True, exist_ok=True)
    connection = sqlite3.connect(database)
    connection.execute("CREATE TABLE lexicons (id TEXT)")
    connection.execute("CREATE TABLE filler (body TEXT)")
    connection.executemany("INSERT INTO filler VALUES (?)", [("x" * 500,)] * 400)
    if mode != "missing":
        connection.execute("INSERT INTO lexicons VALUES ('oewn:2025+')")
    connection.commit()
    connection.close()
    if mode == "journal":
        database.with_name(database.name + "-journal").write_bytes(b"")
    if mode == "corrupt":
        with database.open("r+b") as handle:
            handle.seek(database.stat().st_size - 4096)
            handle.write(b"\xff" * 4096)


if __name__ == "__main__":
    build(Path(sys.argv[1]), sys.argv[2])
`

	stubWordnet = `import os
import sqlite3
from pathlib import Path


class _Config:
    database_path = Path(os.environ.get("FAKE_WN_DIR") or Path.home() / ".wn_data") / "wn.db"


config = _Config()


def lexicons(lexicon):
    connection = sqlite3.connect(config.database_path)
    return [row[0] for row in connection.execute("SELECT id FROM lexicons WHERE id = ?", (lexicon,))]
`

	stubModelCache = `import hashlib
import os
from pathlib import Path

import fixture

WN_SPEC = "oewn:2025+"
WN_ARCHIVE_NAME = "english-wordnet.xml.gz"
WN_ASSET_URL = os.environ["FAKE_ASSET_URL"]
WN_ARCHIVE_SIZE = int(os.environ["FAKE_ARCHIVE_SIZE"])
WN_ARCHIVE_SHA256 = os.environ["FAKE_ARCHIVE_SHA256"]
PROTECTED = os.environ["FAKE_PROTECTED"].split()


def wn_archive_matches(path):
    return (
        path.is_file()
        and path.stat().st_size == WN_ARCHIVE_SIZE
        and hashlib.sha256(path.read_bytes()).hexdigest() == WN_ARCHIVE_SHA256
    )


def ensure_wn_lexicon():
    data = Path.home() / ".wn_data"
    matched = wn_archive_matches(data / WN_ARCHIVE_NAME)
    present = sorted(name for name in PROTECTED if name in os.environ)
    with open(os.environ["FAKE_CALLS"], "a") as calls:
        calls.write(f"ensure archive={matched} keys={present}\n")
    if not matched:
        raise RuntimeError("ensure would fetch the archive through the authenticated helper")
    mode = os.environ["FAKE_WORDNET"]
    if mode == "fail":
        raise RuntimeError("synthetic provision failure")
    if not (data / "wn.db").exists():
        fixture.build(data / "wn.db", mode)
`
)

type wordnetHost struct {
	home     string
	pin      string
	python   string
	stubs    string
	calls    string
	requests atomic.Int64
	server   *httptest.Server
}

func newWordnetHost(t *testing.T, served string) *wordnetHost {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	h := &wordnetHost{home: t.TempDir(), python: python, stubs: t.TempDir(), calls: filepath.Join(t.TempDir(), "calls")}
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		h.requests.Add(1)
		_, _ = w.Write([]byte(served))
	}))
	t.Cleanup(h.server.Close)
	tool := filepath.Join(h.home, ".daemonkit", "tools", "capt-hook", "12.88.9")
	files := map[string]string{
		filepath.Join(h.stubs, "fixture.py"):                             stubFixture,
		filepath.Join(h.stubs, "wn", "__init__.py"):                      stubWordnet,
		filepath.Join(h.stubs, "captain_hook", "__init__.py"):            "",
		filepath.Join(h.stubs, "captain_hook", "util", "__init__.py"):    "",
		filepath.Join(h.stubs, "captain_hook", "util", "model_cache.py"): stubModelCache,
		filepath.Join(tool, ".installed"):                                "",
		filepath.Join(tool, "capt-hook", "bin", "hook"):                  "#!/bin/sh\n",
		filepath.Join(tool, "capt-hook", "bin", "python"):                "#!/bin/sh\nPYTHONPATH=" + h.stubs + " exec " + python + " \"$@\"\n",
	}
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(tool, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "capt-hook", "bin", "hook"), filepath.Join(tool, "bin", "hook")); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *wordnetHost) database(t *testing.T, mode string) {
	t.Helper()
	if out, err := exec.Command(h.python, filepath.Join(h.stubs, "fixture.py"), filepath.Join(h.home, ".wn_data", "wn.db"), mode).CombinedOutput(); err != nil {
		t.Fatalf("build the %s database: %v: %s", mode, err, out)
	}
}

func (h *wordnetHost) run(t *testing.T, mode string, extra ...string) (string, int) {
	t.Helper()
	sum := sha256.Sum256([]byte(wordnetArchive))
	cmd := exec.Command("sh", "-c", wordnetScript("12.88.9", cmp.Or(h.pin, hex.EncodeToString(sum[:]))))
	cmd.Env = append([]string{
		"HOME=" + h.home,
		"PATH=" + os.Getenv("PATH"),
		"FAKE_ASSET_URL=" + h.server.URL + "/english-wordnet.xml.gz",
		"FAKE_ARCHIVE_SIZE=" + strconv.Itoa(len(wordnetArchive)),
		"FAKE_ARCHIVE_SHA256=" + hex.EncodeToString(sum[:]),
		"FAKE_CALLS=" + h.calls,
		"FAKE_PROTECTED=" + strings.Join(orca.CredentialEnv(), " "),
		"FAKE_WORDNET=" + mode,
	}, extra...)
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return string(output), 0
	case errors.As(err, &exit):
		return string(output), exit.ExitCode()
	}
	t.Fatal(err)
	return "", 0
}

func (h *wordnetHost) ensured() string {
	calls, _ := os.ReadFile(h.calls)
	return string(calls)
}

const ensuredOnce = "ensure archive=True keys=[]\n"

func TestWordnetScriptSeedsThePublicArchiveThenProvisionsAndChecks(t *testing.T) {
	h := newWordnetHost(t, wordnetArchive)
	output, code := h.run(t, "complete")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, output)
	}
	if got := h.ensured(); got != ensuredOnce {
		t.Errorf("ensure_wn_lexicon calls = %q, want one call that finds the seeded archive", got)
	}
	if h.requests.Load() != 1 {
		t.Errorf("downloaded the archive %d times, want once", h.requests.Load())
	}
	archive, err := os.ReadFile(filepath.Join(h.home, ".wn_data", "english-wordnet.xml.gz"))
	if err != nil || string(archive) != wordnetArchive {
		t.Errorf("seeded archive = %q, %v", archive, err)
	}
	entries, _ := os.ReadDir(filepath.Join(h.home, ".wn_data"))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			t.Errorf("a partial download %s survived", entry.Name())
		}
	}
	if output, code := h.run(t, "complete"); code != 0 || h.requests.Load() != 1 || h.ensured() != ensuredOnce+ensuredOnce {
		t.Errorf("a prepared host ran again with exit %d (%s), %d downloads, ensure calls %q", code, output, h.requests.Load(), h.ensured())
	}
}

func TestWordnetScriptKeepsAgentKeysOutOfTheInstalledPython(t *testing.T) {
	h := newWordnetHost(t, wordnetArchive)
	keys := make([]string, 0, len(orca.CredentialEnv()))
	for _, name := range orca.CredentialEnv() {
		keys = append(keys, name+"="+keySentinel)
	}
	output, code := h.run(t, "complete", keys...)
	if code != 0 || h.ensured() != ensuredOnce || strings.Contains(output, keySentinel) {
		t.Errorf("exit %d (%q), ensure calls %q: want no agent key in the installed Python's environment or output", code, output, h.ensured())
	}
}

func TestWordnetScriptVerifiesAnExistingDatabaseBeforeItWrites(t *testing.T) {
	tests := []struct {
		name, mode, want string
	}{
		{"corrupt", "corrupt", "fails quick_check: database disk image is malformed"},
		{"unfinished write", "journal", "has an unfinished write (-journal)"},
		{"not a file", "directory", "is not a regular file"},
		{"healthy", "complete", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newWordnetHost(t, wordnetArchive)
			if tt.mode == "directory" {
				if err := os.MkdirAll(filepath.Join(h.home, ".wn_data", "wn.db"), 0o700); err != nil {
					t.Fatal(err)
				}
			} else {
				h.database(t, tt.mode)
			}
			before, _ := os.ReadFile(filepath.Join(h.home, ".wn_data", "wn.db"))
			output, code := h.run(t, "complete")
			if tt.want == "" {
				if code != 0 || h.ensured() != ensuredOnce {
					t.Fatalf("exit %d (%s), ensure calls %q; want a healthy database verified and kept", code, output, h.ensured())
				}
				return
			}
			if code != 1 || !strings.Contains(output, tt.want) {
				t.Fatalf("exit %d with %q, want exit 1 naming %q", code, output, tt.want)
			}
			after, _ := os.ReadFile(filepath.Join(h.home, ".wn_data", "wn.db"))
			if h.ensured() != "" || h.requests.Load() != 0 || string(after) != string(before) {
				t.Errorf("a refused database reached ensure (%q), downloaded %d times, or changed", h.ensured(), h.requests.Load())
			}
		})
	}
}

func TestWordnetScriptRefusesAnOrphanSidecarBeforeItCreatesADatabase(t *testing.T) {
	for _, suffix := range []string{"-journal", "-wal"} {
		t.Run(suffix, func(t *testing.T) {
			h := newWordnetHost(t, wordnetArchive)
			data := filepath.Join(h.home, ".wn_data")
			sidecar := filepath.Join(data, "wn.db"+suffix)
			if err := os.MkdirAll(data, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(sidecar, []byte("orphan "+suffix+" bytes"), 0o600); err != nil {
				t.Fatal(err)
			}
			output, code := h.run(t, "complete")
			if code != 1 || !strings.Contains(output, "has an unfinished write ("+suffix+")") {
				t.Fatalf("exit %d with %q, want exit 1 naming the orphan %s", code, output, suffix)
			}
			if h.ensured() != "" || h.requests.Load() != 0 {
				t.Errorf("an orphan %s reached ensure (%q) or downloaded %d times", suffix, h.ensured(), h.requests.Load())
			}
			if kept, err := os.ReadFile(sidecar); err != nil || string(kept) != "orphan "+suffix+" bytes" {
				t.Errorf("the orphan %s changed: %q, %v", suffix, kept, err)
			}
			if _, err := os.Lstat(filepath.Join(data, "wn.db")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a database was created beside the orphan %s: %v", suffix, err)
			}
		})
	}
}

func TestWordnetScriptRefusesWhatItCannotVerify(t *testing.T) {
	tests := []struct {
		name, mode, want, ensured string
		served                    string
		extra                     []string
		uninstall                 bool
		pin                       string
	}{
		{name: "missing lexicon", mode: "missing", want: "holds no oewn:2025+ lexicon", ensured: ensuredOnce},
		{name: "unfinished write", mode: "journal", want: "has an unfinished write (-journal)", ensured: ensuredOnce},
		{name: "corrupt database", mode: "corrupt", want: "fails quick_check: database disk image is malformed", ensured: ensuredOnce},
		{name: "failed provision", mode: "fail", want: "RuntimeError: synthetic provision failure", ensured: ensuredOnce},
		{name: "mismatched archive", mode: "complete", served: "tampered archive", want: "is not " + strconv.Itoa(len(wordnetArchive)) + " bytes with sha256"},
		{name: "data dir override", mode: "complete", extra: []string{"WN_DATA_DIR=/elsewhere"}, want: "WN_DATA_DIR moves WordNet off ~/.wn_data"},
		{name: "wn resolves elsewhere", mode: "complete", extra: []string{"FAKE_WN_DIR=/elsewhere"}, want: "WordNet resolves /elsewhere/wn.db"},
		{name: "tool env not installed", mode: "complete", uninstall: true, want: "the capt-hook 12.88.9 tool env under"},
		{name: "another pinned archive", mode: "complete", pin: strings.Repeat("0", 64), want: "Captain Hook pins the WordNet archive sha256"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newWordnetHost(t, cmp.Or(tt.served, wordnetArchive))
			h.pin = tt.pin
			if tt.uninstall {
				if err := os.Remove(filepath.Join(h.home, ".daemonkit", "tools", "capt-hook", "12.88.9", ".installed")); err != nil {
					t.Fatal(err)
				}
			}
			output, code := h.run(t, tt.mode, tt.extra...)
			if code != 1 || !strings.Contains(output, tt.want) {
				t.Fatalf("exit %d with %q, want exit 1 naming %q", code, output, tt.want)
			}
			if got := h.ensured(); got != tt.ensured {
				t.Errorf("ensure_wn_lexicon calls = %q, want %q", got, tt.ensured)
			}
			if tt.served != "" {
				if _, err := os.Stat(filepath.Join(h.home, ".wn_data", "english-wordnet.xml.gz")); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("a mismatched download was kept: %v", err)
				}
			}
		})
	}
}
