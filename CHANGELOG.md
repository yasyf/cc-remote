# Changelog

All notable changes to this project are documented here.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed
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
