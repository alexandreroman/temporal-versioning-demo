---
name: "Casper workspace integration"
description: "Casper wiring lives in .casper.json; the repo stays tool-agnostic except one guarded Makefile trio, and the info panel covers the non-k8s flows only"
type: feedback
---

# Casper workspace integration

Workspace-specific wiring lives in `.casper.json` alone; every file the demo
itself ships stays tool-agnostic. `.casper.json` maps the Casper lifecycle onto
ordinary Make targets and copies `.env` / `.env.local` into a fresh workspace,
and `CASPER_PORT` reaches the stack through `make worktree-ports`, which is the
only place a workspace's assigned port enters the build.

`make endpoints` is the single source of truth for the demo's addresses: it
prints Markdown on stdout, so the same output reads in a terminal and pipes
into the info panel. `APP_PORT` selects which dashboard port it advertises —
the Docker stack's published port by default, the Air live-reload proxy port
when `dev` overrides it.

The only Casper-aware part of the Makefile is the guarded trio
`in-casper-workspace` / `publish-endpoints` / `clear-endpoints`. The guard
tests both `CASPER_WORKSPACE_ID` and the presence of the `casper` CLI, and it
belongs in the Makefile — `.casper.json` is read by Casper alone, so a CLI
check there is noise.

The info panel covers the flows that run outside Kubernetes: `app-up`,
`app-v1`/`-v2`/`-v3` and `dev` publish the endpoints, `app-down` and
`dev-stop` clear them. The `deploy` / `deploy-vN` / `teardown` targets never
touch the panel — a cluster deployment publishes no host address the panel
could advertise.

**Why:** the demo has to read as an ordinary Temporal demo to someone who has
never heard of the workspace tool, and the panel is worth trusting only if it
tells the truth whatever command was typed.

**How to apply:** put workspace-specific wiring in `.casper.json` and keep the
repository's own files tool-agnostic. A new target that brings the host-side
endpoints up or takes them down publishes or clears the panel too; a target
that only touches the cluster leaves it alone. See [[makefile-target-naming]]
and [[verifying-rollout-across-modes]].
