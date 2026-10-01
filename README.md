# ![cc-remote](docs/assets/readme-banner.webp)

**Remote agent workspaces, ready with your tools.**

[![CI](https://img.shields.io/github/actions/workflow/status/yasyf/cc-remote/ci.yml?branch=main&label=ci)](https://github.com/yasyf/cc-remote/actions/workflows/ci.yml)
[![License: PolyForm-Noncommercial-1.0.0](https://img.shields.io/badge/License-PolyForm--Noncommercial--1.0.0-blue.svg)](https://github.com/yasyf/cc-remote/blob/main/LICENSE)

cc-remote is planned as a repo-agnostic CLI to create and manage VMs where Claude
Code and Codex run against a repository checkout. Planned backends are Sprites and
Namespace, with Orca as a planned desktop frontend. The default workspace will
contain agent tools and plugins plus a shallow checkout of the requested ref,
without project dependency installation or Tilt. A full-stack workspace on
Namespace over SSH will require opt-in config.

Status: early development. The Sprites and Namespace providers and tool inventory
have landed; workspace lifecycle and Orca integration are in progress. The
public CLI exposes `version` and `images render|fingerprint|build`.
See [docs/architecture.md](docs/architecture.md) for the intended architecture.

The [tool inventory reference](docs/tool-inventory.md) lists image, tool, plugin,
and service fields and the available image commands.

## Build from source

```bash
go install github.com/yasyf/cc-remote/cmd/cc-remote@latest
cc-remote version
```

Licensed under [PolyForm-Noncommercial-1.0.0](LICENSE).
