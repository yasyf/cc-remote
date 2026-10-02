# Tool inventory reference

The inventory is one YAML document with `version: 1`. Unknown keys, unpinned
artifact downloads, overlapping installation directories, and duplicate executable
names are rejected. [examples/inventory.yaml](../examples/inventory.yaml) contains
every supported section. Its example marketplace and tool values are placeholders
to replace before installation; rendering and fingerprinting need no provider sign-in.

## CLI commands

| Command | Required flags | Result |
| --- | --- | --- |
| `cc-remote images render` | `--inventory`, `--profile`, `--out` | Writes `plugins.sh` and `provision.sh`; with `image`, also writes `Dockerfile` and `start.sh`. |
| `cc-remote images fingerprint` | `--inventory`, `--profile` | Prints JSON with `tools`, and `image` when configured. |
| `cc-remote images build` | `--inventory` | Builds and publishes the configured Namespace image through `devbox image build`. Requires a logged-in `devbox` CLI. |
| `cc-remote payload build` | `--out` | Builds a private `SquashFS` payload on a fresh Sprite. Accepts `--config`, `--provider`, and `--profile`; prints JSON with `sha256`, `size`, `tools`, `machine`, and `path`. |

The committed example has this fingerprint output:

```sh
cc-remote images fingerprint --inventory examples/inventory.yaml --profile agents
```

```json
{
  "tools": "d3f5cab0276494d302c92126202e35f4726e96d50aa8cb222596c91a1f9e7b82",
  "image": "283bddd21100faa26510aa42b58319ecdd9cffbf11e2ed8478d0b88189751cc4"
}
```

## Inventory sections

| Section | Fields | Meaning |
| --- | --- | --- |
| `image` | `name`, `base`, `user`, `workspaceDir`, `layer` | Optional Namespace image. `base` requires an `@sha256:` digest; `workspaceDir` is an absolute path. `layer` adds single-line Dockerfile instructions after provisioning and forbids `FROM`, continuations, and heredocs. |
| `apt` | `install`, `t64`, `remove`, `payload` | Root package lists. `t64` accommodates the distribution's package suffix. `payload` is required when building or mounting a verified payload. `resident` names the packages every machine installs with their maintainer scripts. The payload build downloads the resident transaction once, records the build machine's installed packages as its base, and writes the `.deb` files with a `debs.json` manifest into a separate packages archive, outside the SquashFS, whose sha256 the payload manifest records; `payload build --packages-out` saves it, and the machine's `payload.packages` pins it like the payload itself, as a `path` to stream or a `url_command` with a `size`. A fresh machine admits the archive under `/var/lib/cc-remote/packages` only when its sha256 matches, extracts it, and installs it with `apt-get install --no-download` after checking each sha256 and its own installed packages against that base, without waiting for the payload mount, and runs no `apt-get update`. The payload phase fails when the mounted manifest names another archive. A base that differs fails before apt runs and needs a payload rebuilt on the new base; there is no online fallback. A closure payload declares `schemaVersion` 2. `closure` names the packages the payload build captures from a full install into `/opt/cc-remote/closure`, which a machine loads through `ld.so.conf.d` and `fonts/conf.d` entries; `bins` are closure executables linked into `/usr/local/bin`; `fonts` are families readiness proves with `fc-match`; `consumers` are home-relative or `/opt/cc-remote` executables whose libraries readiness lists with the real loader; `projections` are closure directories linked at their compiled-in paths (a closure copy that is itself a symlink must resolve inside the closure); a resident package that later installs a real directory at a projected path makes the next activation fail closed. Resident mode fails when any `closure` package is installed, naming each with its dpkg status and version, so a machine provisioned in full mode stays on full mode and is never converted; the closure targets fresh payload machines and their same-mode resumes. With a closure, the tool and plugin installs check pins and link spelling only; the executable probes run at publish, after the loader has registered the closure, and the boot remount re-runs `ldconfig` only once that registration has happened. Without `payload`, the rendered scripts are unchanged. |
| `system` | Artifact list | Root artifacts under `/opt/cc-remote/tools`, linked into `/usr/local/bin`. |
| `tools` | Artifact list | User artifacts under `$HOME/.local/share/cc-remote/tools`, or their explicit `dest`, linked into `$HOME/.local/bin`. |
| `links` | Executable names | Adds user links to installed system executables. |
| `python` | `version`, `system`, `user` | System Python tools and user tool launchers through `uv`; fields are below. |
| `claude` | `managedSettings`, `env`, `marketplaces`, `plugins` | Claude settings and pinned marketplace/plugin inventory. |
| `codexRuntime` | `version`, `url`, `sha256`, `plugins` | Pinned Codex runtime archive and selected runtime plugin names. Requires the `codex` executable in the artifact inventory. |
| `captainHook` | `version`, `url`, `sha256` | Pinned Captain Hook host archive, installed through `capt-hookd package-install`. Requires `uv`. |
| `cookiesync` | `schemaFingerprint` | Expected schema fingerprint; requires the `cookiesync` executable. Configuration writes `synckit/state.json` at this fingerprint if missing, before running `configure.run` commands or starting services. Configuration fails unless the file contains exactly one `synckit-state-v1` document at this fingerprint. |
| `services` | Service list | User services started during configuration; fields are below. |
| `prepare` | Shell command list | Runs as the user from `$HOME` after installation, before verification. Empty by default. |
| `configure` | `env`, `run` | Declared runtime environment names and shell commands run during configuration. |
| `profiles` | Map of names to `tools` and `prepare` | Adds tools and preparation commands for the selected profile. The base inventory always applies. |

## Artifact fields

| Field | Contract |
| --- | --- |
| `name` | Artifact name starting with a letter or digit; only letters, digits, `.`, `_`, and `-` are allowed. |
| `version` | Pinned version string. |
| `url` | HTTPS download URL. |
| `sha256`, `sha512` | Exactly one is required, as lowercase hex of the appropriate length. |
| `format` | `binary`, `gzip`, `tar.gz`, `tar.xz`, `zip`, or `deb`. A `deb` belongs under `system`. |
| `dest` | Optional user destination relative to `$HOME`. It must be a clean path and may not overlap another artifact or managed state directory. |
| `bins` | Map of executable names to extracted paths. Archives require relative member paths; Debian packages require absolute paths. For a binary or gzip, the extracted file is `name`. |
| `verify` | Arguments used to check the linked executable, such as `[--version]`. |

Non-Debian artifacts install with at most four concurrent jobs per phase. Each
Debian artifact installs serially in the `packages` phase. Each phase drains its
artifact jobs before verification. A failed job drains the started jobs and
prevents readiness.

Every download is digest-checked before installation. Verification checks its
digest marker and executable link target. Profile tools cannot overwrite base
tools' executables or extraction directories.

## Python, plugins, and services

Python tools use `name`, `package`, `version`, `marketplace`, `args`, `bins`, and
`verify`. `package` may include extras. Exactly one of `version` or `marketplace`
is required. Marketplace sources belong under `python.user`; `python.system`
requires a system `uv` artifact. Every Python tool declares its executable `bins`.

`python.system` installs tool environments during provisioning and runs
their `verify` arguments. `python.user` creates a launcher for each declared bin
without installing a tool environment. Each launcher runs `uv tool run` with the
configured Python version, package extras, source pin, and `args`, followed by the
bin name and the caller's arguments. The first invocation materializes the tool
environment. Verification compares the executable launcher with its expected
contents and does not run the tool or its `verify` arguments.

| Entry | Fields |
| --- | --- |
| `claude.marketplaces[]` | `name`, `github` as `owner/repo`, exactly one of `ref` or `branch`, and optional `private` for `ref` only. |
| `claude.plugins[]` | `id` as `name@marketplace`, `version`, and optional `bins` relative to the plugin root. |
| `services[]` | `name`, argument-vector `command`, `ready` as the home-relative path of the control socket that must accept a connection before configuration continues, optional `plugin`, and optional `env` map. A plugin service's command is relative to that plugin's root. |

A `ref` marketplace uses a 40-character commit that cc-remote fetches into its
own checkout for directory registration; Python tools cannot install from a
`branch` marketplace because it has no pinned checkout. A `branch` marketplace
has no commit pin and is registered through Claude Code as `owner/repo#<branch>`
because Claude Code clones sources with `git clone --branch` and cannot use a
40-character commit as a GitHub ref. `claude-plugins-official` publishes no tags
and Claude Code refuses a local-directory source for that reserved name, so it
must use `branch`. On every install run, cc-remote reconciles every
marketplace's registration with the inventory, removing and re-adding entries
whose recorded source differs in type, repo, branch, or directory path,
including Claude Code's automatically added unpinned entry. cc-remote registers
branch entries at the pinned branch before rewriting each branch marketplace's
declaration in `~/.claude/settings.json` under `extraKnownMarketplaces` with
`autoUpdate: false` on every install run because Claude Code auto-updates
`claude-plugins-official` by default. Verify checks both `ref` and `branch`
registrations and fails if a registration or declaration differs; install and
verify reject versions that differ from their plugin pins, so upstream version
bumps fail closed until the inventory is bumped.

Verification also checks each `ref` checkout's commit. Declared plugin binaries
normally run `--version`. A pinned local binrun release launcher instead
receives executable and content checks against its marketplace source, including
the descriptor's version, sizes, and SHA-256 pins. Verification leaves its payload
for first use. Captain Hook binaries retain their runtime probes.

`claude.env` and service `env` values may reference `${NAME}` when `NAME` is
declared in `configure.env`. Configuration requires exactly those declared names
and nonempty values. The install phase reads a GitHub token from stdin for private
marketplace access.

Changing a marketplace ref while keeping its plugin versions unchanged does not
reinstall those plugins. Plugin reinstallation uses the version as its identity.

## Machine payloads

A profile machine can select a private `SquashFS` image with
`profiles.<profile>.machine.<provider>.payload`. Payloads require a machine
provisioned in place with the `sprite-env` supervisor and exclude `image`.
The local `path` source remains the default; `url_command` opts into a direct
HTTPS download by the machine. Exactly one of `path` and `url_command` must be set.

| Field | Contract |
| --- | --- |
| `path` | Must resolve to a local regular file owned by the current user, with no group or other permissions; symlinks are followed, and relative paths resolve from the config file's directory. Other file types fail creation before streaming. |
| `url_command` | YAML list whose first element names an executable and whose remaining elements are its arguments. The executable resolves relative to the config file's directory or uses an absolute path; it is never searched for on `PATH`. |
| `sha256` | File digest as 64 lowercase hexadecimal characters. |
| `size` | Positive byte count pinned for a direct download. Required with `url_command`; forbidden with `path`. |

cc-remote appends `sha256` and decimal `size`, in that order, to the configured
`url_command` arguments. It runs the command in the config file's directory with
empty stdin and a 60 s timeout. On create, it runs after the name and tailnet
checks, before saving the workspace record or creating a machine. On a resume
that needs tool reinstallation because the readiness stamp changed, it runs
again immediately before the reinstall.

The command's output contract is:

| Output | Contract |
| --- | --- |
| Stdout | Exactly one absolute `https://` URL with a host and no userinfo, optionally followed by one newline. The URL is at most 8192 bytes of printable ASCII, with no spaces, double quotes (`"`), or backslashes (`\`). |
| Stderr | Discarded on success, failure, timeout, and kill; never reaches the CLI, Orca, slog, errors, state, or results. The command must never print the URL on stderr. |
| Exit 0 | Supplies the URL on stdout; malformed output still fails creation before a machine exists. |
| Nonzero exit | Fails creation before a machine exists, reported only as `payload url_command <argv0>: <exit status or signal>`. A timeout also fails at this point. |

A site command can use `aws s3api head-object` to confirm the object exists and
its `ContentLength` equals the supplied `size`, returning exit status `3`
otherwise. It then runs
`aws s3 presign` with `--expires-in 900` and prints the URL. The URL must use the
regional endpoint to avoid a `301` or `307` redirect and remain valid for at least
600 s.

The helper should put diagnostics, such as AWS SSO sign-in guidance, in its own log
or a message it prints when run by hand. It must print the URL only on stdout.

The URL is a bearer credential. cc-remote keeps it only in memory and sends it to
the machine solely on the stdin of one remote command, as the curl config
`url = "..."`. It never appears in process arguments or environment, logs, state,
result JSON, or error strings. Every `fmt` verb formats the Go URL value as
`[payload url]`.

With a configured Sprites provider and `lean` profile, a build command is:

```sh
cc-remote payload build --config ./config.yaml --provider sprites --profile lean --out ./tools.sqfs
```

The build installs the full inventory, including private marketplaces and the
selected profile's additions, on a fresh build Sprite. Private marketplace access
uses `GH_TOKEN`, `GITHUB_TOKEN`, or `git.token_command`; the token reaches the
Sprite only on the install phase's stdin.

Before packing, `plugins.sh verify` checks system and user tools and links,
including running each target's verify arguments; a failed check stops the build.
The build packs an explicit allowlist of installed trees, streams the file back,
and computes its SHA-256 and byte size.
The build refuses to create `--out` if it exists or its parent directory is
writable by group or others; the new file has mode `0600`. The build destroys its
Sprite and confirms its absence before returning success. Its returned `path` and
`sha256` supply the machine's payload fields.

With `path`, the Sprite runs
`sudo bash -c <stage script> stage-payload <sha256>` with the laptop file on the
provider's exec stdin. The script runs `cat | tee <staging file> | sha256sum`,
writing and hashing the bytes in one pass. It checks every pipeline stage's exit
status and compares the digest to the pin before renaming the staging file to
`/var/lib/cc-remote/payload/<sha256>.sqfs.admitted` under the store lock (`.lock`).

With `url_command`, the machine runs
`sudo bash -c <fetch script> fetch-payload <sha256> <size>`. The script runs
`curl -q --config - --silent --fail --proto =https --max-filesize <size>` with a
30 s connection timeout and a 600 s transfer timeout. It pipes the response
through `tee` into native `sha256sum`, writing the staging file and hashing the
bytes in one pass. The checksum overlaps the transfer; the downloaded file is
never re-read for admission. The script checks every pipeline stage's exit
status, requires HTTP `200`, and compares the file's size and SHA-256 with both
pins. Only then does it rename the staging file to `<sha256>.sqfs.admitted` under
the store lock (`.lock`).

HTTP errors, including `403` and `404`, fail with the HTTP code; a `403` diagnostic
notes that the presigned URL may have expired. Redirects are not followed.
Truncation, a failed pipeline stage, a timeout, a size mismatch, or a digest
mismatch fails the transfer.

Both scripts enable `pipefail` and use `mktemp` to create a file with mode `0600`
named `<sha256>.sqfs.<8 random chars>.partial` in the store for each invocation.
A canceled transfer that is still running cannot overwrite another transfer's
bytes. Their `EXIT` traps remove staging files on failure. Both sources report a
digest mismatch as
`cc-remote: the payload has sha256 <got>, want <pinned>`.

Both sources run inside the existing tools lane, concurrently with checkout and
packages. No additional lane is created. The `provision.sh payload` phase accepts
these forms under `/var/lib/cc-remote/payload`:

| File | Admission before mount |
| --- | --- |
| `<sha256>.sqfs.admitted` | Already verified during transfer; renamed to `<sha256>.sqfs` under the store lock without a second hash. |
| `<sha256>.sqfs` | Cached file; hashed before mount. |

The payload phase never reads `.partial` files. Without an admitted or cached
image, it fails with `cc-remote: no payload is admitted at <image>.admitted`.

The payload phase mounts the image read-only under
`/opt/cc-remote/payload/<sha256>`. An existing mount is accepted only when the loop
device behind it hashes to the digest, even if a staged file replaced the image.
A digest mismatch fails creation. Source selection leaves the local file's owner
and mode checks and the mounted image's manifest checks unchanged.

The manifest `cc-remote-payload.json` must have `schemaVersion: 2` and match the
rendered tools fingerprint, the target account's passwd home directory, `uname -m`,
and the OS `VERSION_ID`. A mismatch or missing required tree fails creation.
Packing also fails when a required tree is missing on the build Sprite.
The direct download admission logic in `provision.sh` changes the tools
fingerprint. Payloads built before this change fail the manifest check with
`has tools "<old>", want "<new>"` and require a rebuild.

The allowlist uses the following exposure rules for entries in the inventory:

| Trees | Exposure |
| --- | --- |
| Non-Debian system artifacts, user artifacts, and profile artifacts | Link each versioned directory or user `dest` to its payload tree. |
| System Python tool environments | Link each `/opt/uv/tools/<name>`; keep `/opt/uv/tools` writable. Copy their `/usr/local/bin` entrypoint symlinks. |
| `/opt/uv/python` and `$HOME/.local/share/uv/python` | Link each whole root. The user Python root is the only optional tree. |
| Pinned `ref` marketplace checkouts, public or private | Link each checkout. |
| Branch marketplace checkouts and every plugin cache version, public or private | Copy into writable directories. |
| Claude's `installed_plugins.json`, `known_marketplaces.json`, and `settings.json` | Copy as regular files; pack `settings.json` when the inventory pins Claude plugins or declares a branch marketplace. |
| Codex runtime and configuration | Link `$HOME/.cache/codex-runtimes/codex-primary-runtime`; copy `$HOME/.codex/config.toml` and `$HOME/.codex/plugins/cache/openai-primary-runtime`. |
| Captain Hook version directory | Keep a real directory; link its children except `.lock`. |

Exposure replaces links into older payloads, including child links in an existing
Captain Hook version directory, and leaves other existing paths, including
copies, to the installer.

Before installing plugins on create or resume with a payload, cc-remote merges
the payload's `enabledPlugins` into an existing `~/.claude/settings.json`,
preserving every other user setting. The merge rejects either file unless it
contains exactly one JSON object with `enabledPlugins` absent or an object.
If no user settings file exists, exposure copies the payload's file.

Plugin caches are writable because plugins build runtime files
and Claude writes `.in_use` there. The allowlist excludes owner state: agent
authentication and sessions, plugin data, daemon locks, cc-remote workspace state,
and SSH/tailnet state. Packing also excludes `.in_use` and `.orphaned_at`.

The `cc-remote-payload` service checks each stored payload, or the loop device
behind its existing mount, against its digest and remounts the files at boot.
It verifies every `*.sqfs` and ignores `.partial` and `.admitted` files.
Inventory services registered with `sprite-env` depend on it.

## Script phases

The rendered scripts separate package installation, tool verification, and
readiness publication:

| Invocation | Result |
| --- | --- |
| `provision.sh prerequisites` | Checks for `curl`, `git`, `jq`, `python3`, `unzip`, `xz`, and `/etc/ssl/certs/ca-certificates.crt`; only if any are missing, runs `apt-get update` and installs `ca-certificates`, `curl`, `git`, `jq`, `python3`, `unzip`, and `xz-utils`. Runs no apt command when all exist. |
| `provision.sh packages` | Installs prerequisite packages, inventory packages, and Debian artifacts with `apt-get`, verifies the artifacts, and clears package lists. With `apt.payload`, `full` also downloads and records the resident transaction for capture, and `resident SHA256` installs the captured `.deb` files offline from that mounted payload. |
| `provision.sh tools` | Installs and verifies non-Debian system artifacts and system Python tools; writes managed Claude settings. |
| `provision.sh payload SHA256 FINGERPRINT` | Accepts `<sha256>.sqfs.admitted` without a second hash or hashes a cached `<sha256>.sqfs`; fails if neither exists. Mounts the payload, validates its manifest, registers boot remounting, and exposes system trees. Existing mounts require a loop-device hash check. |
| `provision.sh pack FINGERPRINT` | Packs the allowlisted trees and manifest into a `SquashFS` file with zstd compression. |
| `plugins.sh install [PAYLOAD_DIR]` | Clears readiness, optionally exposes home trees, reconciles tools and plugins, runs inventory preparation, and verifies user tools; links are checked by target path only, since a Debian target may still be installing. |
| `plugins.sh publish STAMP` | Verifies system tools and user links, including that each target runs its verify arguments, then writes the readiness stamp. |
| `plugins.sh ready STAMP` | Checks the readiness stamp. |
| `plugins.sh configure` | Applies the declared environment, runs configuration commands, and starts services. |
| `plugins.sh verify` | Verifies both system and user tools. |

Namespace image builds run `packages` followed by `tools`.

## Fingerprints and readiness

| Value | Inputs |
| --- | --- |
| Tool fingerprint | Rendered `provision.sh` and `plugins.sh` for the selected profile, including its tool and prepare additions. |
| Image fingerprint | Rendered `Dockerfile`, system-only `provision.sh`, and `start.sh`; user artifact and plugin versions are excluded. |
| Readiness stamp | Tool fingerprint, plus the image fingerprint for an image-backed host or the configured payload `sha256` for a machine with a payload, plus `payload.packages.sha256` when the payload pins a packages archive; payload and archive `path`, `url_command`, and `size` are excluded. |

Each uses SHA-256 with its own domain and file-name/length framing. The image
description includes `cc-remote-image=<fingerprint>`.

Installation clears readiness and never writes the stamp. Create first runs
`prerequisites` on machines provisioned in place. It then runs packages,
checkout, and tool installation in concurrent lanes. The tools lane includes
payload mounting when configured. Profile preparation (when nonempty) and
configuration wait for both installs to succeed; tailnet enrollment follows
configuration.
Only after every lane succeeds does `publish` write the stamp; connection follows.

On resume, tool reinstallation also runs `prerequisites` first on machines
provisioned in place. It runs packages and tools concurrently, then publishes
before profile preparation and configuration. The `ready` phase checks the stamp.
A changed payload `sha256` causes a readiness miss on resume, which stages,
verifies, and mounts the new payload before publishing.

Namespace image building and live host startup have not been exercised with this
implementation. CI grades rendering, validation, shell syntax, shellcheck, Python
compilation, and fixture-based installation behavior.
