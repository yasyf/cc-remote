# Changelog

All notable changes to this project are documented here.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed
- **The resident package install skips man-db's index update.** After the
  offline install, dpkg runs man-db's trigger, which updates the manual-page
  index that only `apropos` and `whatis` read. The trigger took under 1 s on
  v0.26.10 creates, 31 s on a v0.26.11 fresh create, and 53 s on a v0.26.12
  warm spare whose payload had finished writing to disk 2 s before the trigger
  started. The packages lane sits on a fresh create's critical path. man-db's
  trigger returns without touching the index when `/var/lib/man-db/auto-update`
  is absent, so resident mode now moves that flag aside before the install and
  puts it back once apt returns, whether the install succeeded or failed. The
  workload's later apt runs update the index as the base does, and `man <page>`
  still finds every page. Full mode and the payload build are unchanged. The
  change moves the tools fingerprint, so every payload is rebuilt for this
  release.
- **A payload download writes to disk as it arrives.** The fetch and stage
  scripts wrote the staging file through `tee`, which left the whole download
  dirty in the page cache. A fresh Sprite has 16 GB of memory and a 20%
  `vm.dirty_ratio`, so nothing throttled the writer. On a fresh create the
  2.15 GB payload was in memory 22 s into `payload.fetch`, yet the phase took
  57 s. The admit's `mv` waited on an ext4 journal commit that first had to
  write the payload back. The packages lane's man-db and ldconfig triggers
  stalled for 31 s and 8 s behind the same write-back. The scripts now write
  the staging file with `dd oflag=direct` in 32 MiB blocks and hash a `tee`
  copy in a process substitution whose exit status they wait for. Direct writes
  on that Sprite ran at about 130 MB/s, faster than the download, so the
  write-back overlaps the transfer and `payload.fetch` should end close to its
  last byte. A transfer error now names `dd`'s exit status beside `tee`'s and
  `openssl`'s. The tools fingerprint covers only `provision.sh` and
  `plugins.sh`, so existing payloads stay valid.
- **A create runs its `url_command`s while the machine is being created.** A
  direct payload's `url_command` and its packages archive's `url_command` used
  to run one after the other before `machine.create`, which put 2 to 4 s of
  presigning in front of every fresh Sprite. They now run side by side and
  beside the machine create, and keep their `payload.url`, `packages.url`, and
  `machine.create` phases on the main lane. A failed command now fails the
  create after the machine exists, and the machine is destroyed the same way a
  failed provision destroys it. A failed machine create cancels both commands.
- **The resident package install no longer waits on dpkg's syncs.** The
  offline install of a payload's resident debs took 59 s for 54 packages on a
  fresh Sprite. dpkg went quiet for 19 to 36 s at a time while the payload
  download or the tool install wrote heavily beside it. `--force-unsafe-io`
  skips the sync after each unpacked file but not the roughly 300 that dpkg
  makes after each status change. `libeatmydata1` is now always a resident
  package, so `packages full` installs it and the packages archive captures its
  deb. Resident mode extracts that deb into its scratch directory and runs the
  offline `apt-get install` with `LD_PRELOAD` pointing at the extracted
  `libeatmydata.so`, so dpkg and the maintainer scripts skip every `fsync`.
  The change moves the tools fingerprint, so every payload is rebuilt for this
  release anyway. A packages archive without a `libeatmydata1` deb fails the
  install and names the package.
- **A Claude worker's Orca runtime starts beside the create or the warm claim.**
  `orca create` and `orca prepare` used to start the runtime after the whole
  workspace was ready. They now start it once the machine's tools are
  installed, or once a claimed warm workspace passes its tools check, while the
  configure, checkout refresh, and tailnet steps run. A runtime that fails to
  start still fails the command after the workspace is ready, and the workspace
  is kept. Codex workers and `--existing` keep the old order.
- **`orca create` and `orca prepare` start the runtime through Orca's own CLI.**
  `orca.entry` now names the runtime tool's `orca` CLI and defaults to
  `squashfs-root/resources/bin/orca-ide`. The launcher runs `<entry> serve`
  and the guest bootstrap helper drives the same entry. The old default,
  `squashfs-root/AppRun serve`, stays inside the Electron process and never
  asks Orca which server to use, so a machine launched that way cannot reach
  the managed server Orca 1.4.224 added. A config that sets `orca.entry`
  explicitly keeps its value.
- **The task record names the managed server.** When the runtime's ready event
  carries `health`, the record stores its build as `managedServer` and
  `orca create`, `prepare`, and `status` print it. The field is absent when
  Orca serves on Electron.
- **An Orca worker commits as the person who launches it.** `orca create` and
  `orca prepare` read `user.name` and `user.email` from the launching machine's
  `git config` and send them down the one-use key pipe. The worker's terminal
  sets them through `GIT_CONFIG_*` variables on both providers. A Namespace
  worker used to have no author at all, so `git commit` failed there. A Sprites
  worker used to commit as the placeholder its base image ships, `Sprite
  <noreply@sprites.dev>`, which the variables now outrank. A launch refuses
  before any provider call when either key is unset and names the missing key.
  The identity reaches no task record, workspace record, receipt, log, or file
  on the machine. The credential helper entries that `orca.keys.github` adds
  move from `GIT_CONFIG_KEY_0` and `_1` to `_2` and `_3`.
- **A Claude worker's CLI no longer updates itself.** The worker's terminal
  exports `DISABLE_AUTOUPDATER=1` before it starts Claude, so the CLI stays at
  the version the machine was built with. A worker left running used to
  install a newer release in the background and ask for a restart. The
  variable is Claude Code's own switch for background updates, and a manual
  `claude update` still works. Codex workers are unchanged.
- **A failed warm pool fill records where it failed and how.** The pool record
  of a failed create named a step only when the Sprite create itself failed. A
  command that failed on the machine left no exit code. A fill that died while
  mounting the payload therefore left a length and a hash. The `step` field now
  also names the create phase, such as `payload.mount` or `checkout`. `exit`
  holds the exit code of the command that phase ran. A new `class` says whether
  the error was an exit, an HTTP status, the deadline, a cancel, or something
  else. The error's text is still never stored.

### Added
- **`orca.keys.github` lets an Orca worker push its branch and open its PR.**
  When the config names a command for `github`, cc-remote runs it beside the
  other key commands and sends its output down the same one-use key pipe. The
  worker's terminal exports it as `GH_TOKEN` and points git's credential helper
  for `https://github.com` at `gh auth git-credential` through `GIT_CONFIG_*`
  variables, so `git push` and `gh pr create` work without writing a token or a
  config file on the machine. Codex keeps the token in its shell tools, since
  pushing is the point; the model keys stay excluded. A failing command stops
  the launch, and leaving it unconfigured changes nothing.
- **`orca.keys.typesafe` hands every Orca worker a TypeSafe key.** When the
  config names a command for `typesafe`, cc-remote runs it beside the agent
  and judge key commands, sends its output down the same one-use key pipe,
  and the worker's terminal exports it as `TYPESAFE_API_KEY` before it starts
  Claude or Codex. That is the variable capt-hook's `evt.decide` reads on an
  API actor. The key joins the credential set that Codex's shell policy
  excludes and that the pool fill and grant probes strip. A failing command
  stops the launch, the same as a failing judge key, and leaving it
  unconfigured changes nothing.

### Fixed
- **`cc-remote destroy` removes the warm pool record of the name it destroys.**
  Destroy deleted the workspace record and left `pool/<name>.json` behind, so
  `orca pool status` went on listing machines that were long gone; one state
  directory listed 17 of them. Destroy now removes the pool record together
  with the workspace record. For a name that has only a pool record, destroy
  used to refuse. It now removes the pool record, logs that it touched no
  machine, and still revokes a tailnet node an earlier attempt left bound.
  Nothing prunes pool records on its own.
- **`orca prepare --warm` no longer spends a warm workspace on a helper pin it
  cannot use.** A CLI whose release differed from `orca.bootstrap_helper`
  claimed a ready workspace first and compared the pin afterwards, so the
  mismatch left that workspace `claimed` and `failed` for good. A create
  already refused before it made anything. `--warm` now makes the same
  comparison before it claims, and the pool is untouched.
- **A Codex worker prepares on Orca's managed server again.** The Codex hook
  check accepted one set of thirteen definitions, Orca's eight handlers plus
  Captain Hook's five. Orca's managed server installs no agent hooks, because
  only its Electron main process writes them. A Codex worker on a fresh Orca
  1.4.224 machine therefore failed its bootstrap with
  `missing at SessionStart record 0`. The check now takes its accepted set
  from the server Orca reports in its ready event. On a managed server the set
  is exactly Captain Hook's five, and an Orca handler there is a refusal. On
  Electron it is the same thirteen as before. A pregranted worker on a managed
  server must still hold exactly the hooks its pregrant claimed.
- **`cc-remote destroy` no longer needs the machine's tailnet daemon.** Destroy
  used to ask the guest's `tailscaled` which node it held before removing
  anything, so a machine whose daemon never opened its socket could not be
  destroyed at all. Leaving the tailnet now reads only the workspace's binding
  and the tailnet API. A bound node is deleted through the API after the same
  ownership check as before, and a workspace whose enrollment never registered
  a node has nothing to remove, so destroy goes on to the machine. The
  provider's ownership label check is unchanged.
