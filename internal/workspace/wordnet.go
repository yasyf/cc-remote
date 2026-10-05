package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/yasyf/cc-remote/internal/frontends/orca"
	"github.com/yasyf/cc-remote/internal/remote"
)

const wordnetArchiveSHA256 = "31f4af16c54b532fd5484d4cc33aee588a31bb5b70683ae8197842fde5b586bc"

const wordnetProgram = `import shutil
import sqlite3
import sys
import tempfile
import urllib.request
from pathlib import Path

import wn
from captain_hook.util import model_cache


def finished(database):
    if incomplete := [suffix for suffix in ("-journal", "-wal") if database.with_name(database.name + suffix).exists()]:
        sys.exit(f"cc-remote: {database} has an unfinished write ({', '.join(incomplete)})")


def verify(database):
    try:
        check = sqlite3.connect(f"{database.as_uri()}?mode=ro", uri=True).execute("PRAGMA quick_check").fetchall()
    except sqlite3.DatabaseError as error:
        sys.exit(f"cc-remote: {database} fails quick_check: {error}")
    if check != [("ok",)]:
        sys.exit(f"cc-remote: {database} fails quick_check: {check[:3]}")


if model_cache.WN_ARCHIVE_SHA256 != sys.argv[1]:
    sys.exit(f"cc-remote: Captain Hook pins the WordNet archive sha256 {model_cache.WN_ARCHIVE_SHA256}, not {sys.argv[1]}")
database = Path.home() / ".wn_data" / "wn.db"
if wn.config.database_path != database:
    sys.exit(f"cc-remote: WordNet resolves {wn.config.database_path}, not {database}, the database the actor reads")
if database.is_symlink() or database.exists() and not database.is_file():
    sys.exit(f"cc-remote: {database} is not a regular file")
finished(database)
if database.exists():
    verify(database)
archive = database.parent / model_cache.WN_ARCHIVE_NAME
if not model_cache.wn_archive_matches(archive):
    database.parent.mkdir(exist_ok=True)
    with (
        tempfile.NamedTemporaryFile(dir=database.parent, prefix=f".{archive.name}.", delete=False) as pending,
        urllib.request.urlopen(model_cache.WN_ASSET_URL, timeout=300) as response,
    ):
        shutil.copyfileobj(response, pending)
    seeded = Path(pending.name)
    if not model_cache.wn_archive_matches(seeded):
        seeded.unlink()
        sys.exit(f"cc-remote: {model_cache.WN_ASSET_URL} is not {model_cache.WN_ARCHIVE_SIZE} bytes with sha256 {model_cache.WN_ARCHIVE_SHA256}")
    seeded.replace(archive)
model_cache.ensure_wn_lexicon()
if not wn.lexicons(lexicon=model_cache.WN_SPEC):
    sys.exit(f"cc-remote: {database} holds no {model_cache.WN_SPEC} lexicon")
finished(database)
verify(database)
`

func wordnetScript(version, archive string) string {
	return remote.Script(
		"unset "+strings.Join(orca.CredentialEnv(), " "),
		`tool="$HOME/.daemonkit/tools/capt-hook/"`+remote.Quote(version),
		`test -f "$tool/.installed" || { echo "cc-remote: the capt-hook `+version+` tool env under $tool is not installed" >&2; exit 1; }`,
		`test -z "${WN_DATA_DIR+set}" || { echo "cc-remote: WN_DATA_DIR moves WordNet off ~/.wn_data, the path the actor reads" >&2; exit 1; }`,
		`hook=$(readlink -e "$tool/bin/hook")`,
		`exec "${hook%/*}/python" -P -c `+remote.Quote(wordnetProgram)+" "+remote.Quote(archive),
	)
}

func (s *Session) wordnetContract() string {
	if s.captainHook == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(wordnetScript(s.captainHook, wordnetArchiveSHA256)))
	return hex.EncodeToString(sum[:])
}

func (s *Session) PrepareWordnet(ctx context.Context, machine string) error {
	return s.prewarmWordnet(s.begin(ctx), machine)
}

func (s *Session) prewarmWordnet(ctx context.Context, machine string) error {
	if s.captainHook == "" {
		return nil
	}
	if err := s.timed(ctx, laneWordnet, "wordnet", machine, func() error {
		_, err := s.run(ctx, machine, wordnetScript(s.captainHook, wordnetArchiveSHA256), nil)
		return err
	}); err != nil {
		return fmt.Errorf("prepare WordNet: %w", err)
	}
	s.Log.Info("prepared WordNet", "machine", machine)
	return nil
}
