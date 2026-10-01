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

The committed example has this fingerprint output:

```sh
cc-remote images fingerprint --inventory examples/inventory.yaml --profile agents
```

```json
{
  "tools": "7890c2f4fd24d90aac9888f99cdd64184e0af408b8f38a93d9c1cf672f7b31ac",
  "image": "a377d3eccaf38e09d1e85443838af36aa20136ba24760fb04e58c3188dde8553"
}
```

## Inventory sections

| Section | Fields | Meaning |
| --- | --- | --- |
| `image` | `name`, `base`, `user`, `workspaceDir`, `layer` | Optional Namespace image. `base` requires an `@sha256:` digest; `workspaceDir` is an absolute path. `layer` adds single-line Dockerfile instructions after provisioning. It forbids `FROM`, continuations, and heredocs. |
| `apt` | `install`, `t64`, `remove` | Root package lists. `t64` accommodates the distribution's package suffix. |
| `system` | Artifact list | Root artifacts under `/opt/cc-remote/tools`, linked into `/usr/local/bin`. |
| `tools` | Artifact list | User artifacts under `$HOME/.local/share/cc-remote/tools`, or their explicit `dest`, linked into `$HOME/.local/bin`. |
| `links` | Executable names | Adds user links to installed system executables. |
| `python` | `version`, `system`, `user` | Python tool installations through `uv`; fields are below. |
| `claude` | `managedSettings`, `env`, `marketplaces`, `plugins` | Claude settings and pinned marketplace/plugin inventory. |
| `codexRuntime` | `version`, `url`, `sha256`, `plugins` | Pinned Codex runtime archive and selected runtime plugin names. Requires the `codex` executable in the artifact inventory. |
| `captainHook` | `version`, `url`, `sha256` | Pinned Captain Hook host archive, installed through `capt-hookd package-install`. Requires `uv`. |
| `cookiesync` | `schemaFingerprint` | Expected CookieSync schema fingerprint. Requires the `cookiesync` executable. |
| `services` | Service list | User services started during configuration; fields are below. |
| `prepare` | Shell command list | Runs as the user from `$HOME` after installation, before verification. Empty by default. |
| `configure` | `env`, `run` | Declared runtime environment names and shell commands run during configuration. |
| `profiles` | Map of names to `tools` and `prepare` | Adds tools and preparation commands for the selected profile. The base inventory always applies. |

## Artifact fields

| Field | Contract |
| --- | --- |
| `name` | Artifact name; letters, digits, dots, underscores, and hyphens. Starts with a letter or digit. |
| `version` | Pinned version string. |
| `url` | HTTPS download URL. |
| `sha256`, `sha512` | Exactly one is required, as lowercase hex of the appropriate length. |
| `format` | `binary`, `gzip`, `tar.gz`, `tar.xz`, `zip`, or `deb`. A `deb` belongs under `system`. |
| `dest` | Optional user destination relative to `$HOME`. It must be a clean path and may not overlap another artifact or managed state directory. |
| `bins` | Map of executable names to extracted paths. Archives require relative member paths; Debian packages require absolute paths. For a binary or gzip, the extracted file is `name`. |
| `verify` | Arguments used to check the linked executable, such as `[--version]`. |

Every download is digest-checked before installation. Verification checks its
digest marker and executable link target. Profile tools cannot overwrite base
tools' executables or extraction directories.

## Python, plugins, and services

Python tools use `name`, `package`, `version`, `marketplace`, `args`, `bins`, and
`verify`. `package` may include extras. Exactly one of `version` or `marketplace`
is required. Marketplace sources belong under `python.user`; `python.system`
requires a system `uv` artifact. Every Python tool declares its executable `bins`.

| Entry | Fields |
| --- | --- |
| `claude.marketplaces[]` | `name`, `github` as `owner/repo`, exactly one of `ref` or `branch`, and optional `private` for `ref` only. |
| `claude.plugins[]` | `id` as `name@marketplace`, `version`, and optional `bins` relative to the plugin root. |
| `services[]` | `name`, argument-vector `command`, optional `plugin`, and optional `env` map. A plugin service's command is relative to that plugin's root. |

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

`claude.env` and service `env` values may reference `${NAME}` when `NAME` is
declared in `configure.env`. Configuration requires exactly those declared names
and nonempty values. The install phase reads a GitHub token from stdin for private
marketplace access.

Changing a marketplace ref while keeping its plugin versions unchanged does not
reinstall those plugins. Plugin reinstallation uses the version as its identity.

## Fingerprints and readiness

| Value | Inputs |
| --- | --- |
| Tool fingerprint | Rendered `provision.sh` and `plugins.sh` for the selected profile, including its tool and prepare additions. |
| Image fingerprint | Rendered `Dockerfile`, root `provision.sh`, and `start.sh`. |
| Readiness stamp | Tool fingerprint, plus the image fingerprint for an image-backed host. |

Each uses SHA-256 with its own domain and file-name/length framing. The image
description includes `cc-remote-image=<fingerprint>`. Installation writes the
readiness stamp only after preparation and verification succeed; the `ready`
script phase checks that stamp. Configuration is a separate phase.

Namespace image building and live host startup have not been exercised with this
implementation. CI grades rendering, validation, shell syntax, shellcheck, Python
compilation, and fixture-based installation behavior.
