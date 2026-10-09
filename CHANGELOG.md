# Changelog

All notable changes to this project are documented here.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
