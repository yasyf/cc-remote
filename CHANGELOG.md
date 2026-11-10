# Changelog

All notable changes to this project are documented here.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- **`cc-remote orca stop <lane>` stops the workspace a lane runs on.** It finds
  the workspace `prepare` created under the lane's name or the warm pool
  workspace the lane claimed, closes the task's SSH forward, and suspends the
  machine, keeping its disk and records. It refuses a warm pool workspace's own
  name, so it stops only a workspace a lane claimed.
- **`cc-remote suspend` stops a Sprite.** Sprites has no stop verb, so suspend
  stops every service on the Sprite and waits up to two minutes for it to
  pause, failing with the error when something still holds it.
- **A system `.deb` can be a dependency with no `bins`.** It links nothing, so
  an inventory can pin a backported `git` together with the matching `git-man`
  its `Depends` names.

### Changed
- **A blobless checkout is an opt-in profile value.** A profile's `checkout`
  stays `shallow` by default, a depth-1 clone of the requested ref.
  `checkout: blobless` opts into a clone with the whole history and no file
  contents until a checkout or diff needs them, so a rebase onto an older base
  finds its merge base. A blobless profile can keep a warm pool too.

### Fixed
- **A spare that slept in the warm pool is claimed again.** A Sprite keeps its
  home on a volume of its local disk, which a boot mounts again through a
  service. A spare left unclaimed long enough to sleep boots when the claim
  first reaches it, and the claim checked its checkout before that mount was
  back, so it found no checkout, failed with "the configured project root is
  not the top level of a git checkout", and stayed running as failed. The claim now binds the home
  first, the step a resume already runs, and checks the checkout after it. A
  spare whose home cannot be bound stays running as failed, the same as one
  whose check fails.
- **An Orca runtime that exits starts again.** The launcher ran the runtime in
  place of itself as the `cc-remote-orca` service, and a Sprite counts a service
  as running while any process it started is left. When a Sprite's home volume
  filled and the runtime died on `ENOSPC`, its session daemon and agents stayed,
  so nothing started the runtime again. The launcher now stays and runs the
  runtime in a loop, waiting 1 second before the first restart and doubling up
  to 30 seconds. Before each restart it writes the exit status to `serve.log`,
  and it names the disk and its free MiB when that disk is full. A restarted
  runtime has a new runtime id, which `reconnect` and `status` do not adopt yet.
- **Go's caches leave a Sprite's home volume.** On a Sprite the tool install
  sets `GOMODCACHE`, `GOCACHE` and `GOTMPDIR` under `/var/tmp/cc-remote` on the
  root filesystem. A growing module cache can no longer fill the 12 GiB volume
  the home, `/tmp` and the Orca runtime share. A machine keeps its home caches
  until its tool install runs again.
- **A lane's build can no longer take its Sprite down.** A Sprite's container
  had no memory limit until the platform restarted it, so a Go compile and test
  that reached 7 GB starved the block device daemon behind the root disk; its
  writes timed out, the disk returned I/O errors, and the platform restarted
  every process, which ended the lane. Create, resume and reuse now cap the
  container at 8 GiB with a throttle at 7.5 GiB, and a boot service sets both
  again after every restart, so a build past 7.5 GiB slows down in its own
  container and the disk daemon keeps its memory. Sprite lanes also build Go
  with two jobs and a 2 GiB `GOMEMLIMIT`.
- **One dead machine no longer stalls `orca keep`.** Each forward reopen gets
  one minute, and a forward whose machine still reports its storage gone 10
  minutes after its first failed reopen is released and logged once, so keep
  stops paying about 30 seconds a pass for each machine that will never answer.
- **A full local disk no longer takes a Sprite's root filesystem down.** The
  Sprite's block device daemon keeps its cache on the same 20 GB local disk as
  `/tmp`, and fails every root filesystem write once that disk has less than
  512 MiB free; the platform then remounts the root filesystem and restarts
  every process, and answers `Storage not ready` until space returns. Create
  now mounts a fixed-size volume over the local disk, sized to leave the
  provider 6.5 GiB, so a lane that fills `/tmp` or its home gets `ENOSPC` in its
  own files and the Sprite keeps running.
- **A Codex 0.167 worker gets past its folder-trust screen.** Codex 0.167
  ends that screen with `esc back` and offers "Back to Agent Command Center"
  in place of "Quit", so the startup gate matched nothing and `prepare` timed
  out with "matched no known startup state". The gate now accepts either
  footer and still enters only "1. Trust and continue".
- **A Codex 0.167 worker's hook review reaches the last row.** Codex 0.167
  lists `PreModelRequest` and `PostModelRequest` after `Interrupt` in its
  hooks browser, so the review stopped with "the hooks browser selection moved
  from PreToolUse to PostModelRequest, not its other edge". The review now
  expects those two rows, empty, at the end of the list.
- **A fresh Sprite on Codex 0.159 passes its hook review again.** Since
  0.27.25 the review expected the two model request rows from every worker, so
  a cold launch on an inventory that pins Codex 0.159.2 stopped with "the hooks
  browser selection moved from PreToolUse to Interrupt, not its other edge". A
  warm claim is pregranted and skips the review, so only cold launches failed.
  The review now expects the rows of the Codex release the inventory pins:
  `PreModelRequest` and `PostModelRequest` from 0.164.0 on, when Codex added
  them.
- **A failed warm claim no longer leaves its machine running.** A claim that
  failed before its worker started stayed `failed` with its Sprite running, and
  those Sprites filled the organization's Sprite limit. The claim now checks
  that no agent process runs on the machine, removes its Orca environment,
  suspends it, and records it `stopped`. A machine where an agent runs, one
  that does not answer the check, or one the claim never confirmed unused stays
  running.
- **A warm claim moves past a spare the provider cannot answer for.** One
  ready Sprite whose lookup answered 503 ended the whole `prepare --warm`, even
  with other spares ready. That spare now stays ready and the claim takes the
  next one. The claim fails only when no ready spare could be observed.
- **A lane whose own workspace stopped can be relaunched.** A lane that made
  its own workspace on an empty pool was refused on every relaunch, because its
  task record stayed under the lane's name. `orca stop` now records that task
  as stopped. A relaunch is admitted when the task is stopped or never started
  a worker, and it claims a warm workspace. With none ready, the refusal names
  the cause and says to wait for a pool fill.
- **A lane can be relaunched after a failed launch or a stop.**
  `prepare --warm` refused any lane that had ever claimed a pool workspace. That
  included a claim whose preparation failed before its worker's terminal
  started, and a lane that `cc-remote orca stop` had already stopped, so neither
  could be relaunched. Neither claim holds the lane now: `orca stop` records the
  claim `stopped`, and the relaunch claims the next unused workspace. A lane
  whose worker still runs is refused, and the refusal names the stop that
  frees it.
- **`cc-remote orca stop` succeeds on a Sprite whose storage is gone.** A
  Sprite the provider answers 503 `Storage not ready` for never runs again, and
  stop failed on it every time, so the supervisor kept a stop blocker open. stop
  now names the answer under `found` and exits zero, destroying nothing.
- **`cc-remote orca stop <lane>` reaches the workspace the worker runs on.** A
  lane that moved past a warm workspace that failed during launch holds two pool
  records, and stop read the older one, so it suspended the failed workspace and
  left the worker's running.
- **`cc-remote orca stop` succeeds on a lane that is already gone.** A lane
  with no Orca task record failed with "no Orca task is recorded", and a lane
  whose Sprite no longer existed failed on the suspend. A missing task record
  now only means there is no forward to close. A lane with no workspace record,
  or whose machine the provider no longer has, counts as stopped: stop removes
  its leftover records, names what it found, and exits zero. A machine that
  refuses to suspend still fails the stop.
- **Concurrent fills stop at the ready target.** A fill counted a spare
  another fill finished between its census and its locked recount as neither
  ready nor in flight, so two fills could leave one spare past
  `orca.pool.ready`. A failed create is no longer tried again under a new
  name: the next claim's fill already counts the shortfall and replaces it.
- **A burst of warm claims refills the pool in one create's time.** Every
  `prepare --warm` starts a fill, but fills used to coalesce into one that
  created workspaces one after another, so a burst ran the pool dry for
  minutes and later launches created their workspace cold. Fills now run side
  by side: each counts the workspaces other fills are still creating, reserves
  the shortfall, and creates it at once. A failed create no longer ends the
  fill, and the detached fill's output goes to `pool/fill-<key>.log` instead
  of `/dev/null`.
- **A warm claim that fails before its worker starts moves to the next
  workspace.** A spare whose helper check, Orca pairing, or checkout refresh
  failed stayed claimed as failed and ended the launch. The launch now leaves
  it failed, claims the next unused spare, and creates the lane's own workspace
  once none is left.
- **`cc-remote suspend` passes over a Sprite service that already exited.** The
  Sprites API answers 409 to stopping a service that is not running, which
  ended the suspend and every `cc-remote orca stop` on such a Sprite.
- **Go's caches stay in the home.** A Sprite's Go puts its module cache under
  `/.sprite/languages/go/workspace/pkg/mod`, outside the home volume, and that
  cache served empty files. Builds failed with `expected 'package', found
  'EOF'` and `go.sum` checksum mismatches, even after `go clean -modcache`. The
  tool install now moves a `GOMODCACHE` or `GOCACHE` outside `$HOME` to
  `$HOME/.cache/go-mod` or `$HOME/.cache/go-build` with `go env -w`. A runtime
  the Sprite image ships in `/.sprite/bin`, such as `bun`, is pinned as an
  inventory `tools` entry, since `$HOME/.local/bin` comes first on `PATH`.
- **The Codex runtime extracts outside the home, then moves into place.** On a
  Sprite the home is an overlay filesystem. Under concurrent extraction it
  reported a new inode for the runtime's `share/man/man1` directory, and GNU tar
  failed the payload build with `Directory renamed before its status could be
  extracted`. That directory holds an absolute symlink, so tar sets its status
  last. On a fresh Sprite, four concurrent extractions into the home failed 7
  times in 8, and the same runs into `/tmp`, an ext4 disk, never failed. The
  archive now extracts under the install's temporary directory, and the
  runtime directory is moved into the home.
- **A Namespace create is ready once its container answers ssh.** Create waited
  with `WaitInstanceSync` on the container, and Namespace leaves that status at
  `pulled` when it retries an image pull it cancelled at two minutes. On a 3 GiB
  image every create then failed at 300 s, though the container was running
  from 2:46. Create now waits on the instance alone, which returns as soon as it
  boots, and retries the ownership setup's first container exec until the
  container answers, within `readyTimeout`.
- **A build fails when installing the pinned `.deb` artifacts removes an
  installed package.** `provision.sh packages` lists the installed packages
  before and after the deb transaction and names every one that went missing.
  An image build had no check after the install, so a Namespace image could
  ship without `git-lfs`.
- **A fresh machine installs its pinned `.deb` artifacts with `dpkg -i`.**
  `apt-get install --no-download` on local `.deb` files fails with `Internal
  Error, Pathname to install is not absolute`, so a payload that pins a system
  `.deb` could not create a machine. The resident install's packages are
  already in place, so `dpkg` installs the pinned debs offline in one call.
- **Every system `.deb` installs in one apt transaction.** Installing them one
  at a time let apt resolve a `git-man` upgrade by removing the installed
  `git`, and with it `git-lfs`, before the pinned `git` arrived.

### Changed
- **A Namespace image carries its home in several layers.** The home used to
  be one 1.9 GiB compressed layer, 4.6 GiB unpacked. Namespace's first image
  pull ran past its two-minute cancel partway through unpacking it. The Codex
  runtime and the user tools now each get their own layer, copied from a build
  stage, so each layer unpacks while the next downloads.
- **A create reads the mounted payload into the page cache beside the tools.**
  The payload download writes its staging file with `dd oflag=direct`, so the
  2.15 GB image never enters the page cache and every read of the mounted
  squashfs goes to the Sprite's disk. About one read in ten lands on a slow
  tier. On a fresh Sprite, 400 random 128 KiB direct reads of the payload file
  had a p50 of 0.66 ms, a p90 of 709 ms, and a p99 of 1.5 s. The tool install,
  `plugins.sh configure`, tailscaled, and the Orca runtime all page code in from
  the squashfs at startup, so they stalled together. On v0.26.12 and v0.26.13
  fresh creates `tailnet.enroll` took 31.9 s and 38.3 s and `configure` took
  32.1 s and 40.0 s, against 5.0 s and 4.4 s on v0.26.11, whose buffered write
  left the image cached. Once `payload.mount` succeeds, a new
  `payload.prefetch` phase on its own `prefetch` lane reads the image once with
  `dd bs=32M of=/dev/null` beside the tools lane, and the create joins it with
  the other lanes before publishing. A cold sequential read of the whole image
  took 16.1 s, about 134 MB/s, and a warm re-read took 0.8 s. A failed read
  fails the create the way any failed lane does. The warm pool fills through
  the same create; a warm claim and a resume's reinstall do not prefetch. The
  fetch keeps `oflag=direct`, and the tools fingerprint covers only
  `provision.sh` and `plugins.sh`, so existing payloads stay valid.
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
