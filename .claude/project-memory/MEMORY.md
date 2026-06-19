# Project Memory

> When a new decision **contradicts** an existing memory note, do NOT silently
> override it. Instead: surface the conflict, quote the existing memory, explain
> how the new decision differs, and ask for explicit confirmation before
> updating. **Do NOT take any action** — no tool calls, no file writes — until
> confirmed.

- [Worker versioning model](references/worker-versioning-model.md) — three version-tagged worker images, shape baked at build (`PIZZA_VERSION`); all Pinned; workers publish a `pizzaVersion` label; manual UI routing; bootstrap promotes the v1-labelled version.
- [Version shipping via kustomize base + sibling overlays](references/version-shipping-overlays.md) — base `k8s/base`; `deploy-v2`/`-v3` apply sibling overlays `k8s/vN` (resource `../base` + `images: newTag`) through kbld; `deploy-v1` = base; the sibling layout (not `k8s/` root) avoids the ancestor-cycle.
- [kbld digest pinning for deploy determinism](references/kbld-digest-pinning.md) — every deploy path pipes kustomize through kbld to digest-pin images, so each Build ID maps to one image; pods keep `IfNotPresent`.
- [Prefer short Makefile target names](references/makefile-target-naming.md) — terse targets (`app-vN`, `deploy-vN`); host dev is `make dev` only and runs backend + 3 workers under Air (every component hot-reloads); no alias-only targets.
- [Write project memory in the present tense](references/memory-writing-style.md) — notes state the current truth, not a changelog of what changed; drop "was added/removed", before/after anecdotes.
- [Temporal API constraints](references/temporal-api-constraints.md) — no `.` in deployment name; no `ORDER BY` on dev visibility; visibility omits `VersioningInfo`; ramp/promote need AllowNoPollers + IgnoreMissingTaskQueues; `describe` takes `--name`, `set-*` take `--deployment-name`.
- [Demo timing & v3 drone regression](references/demo-timing.md) — StepDwell 15s, DeliveredDwell 7s; order every ~6s; ramp 25/50/100%; v3 drone always fails via unlimited retry (stays red/Running, never Failed).
- [Workflow waits are activity-side](references/workflow-waits-activity-side.md) — no workflow timers; dwell simulated in activities (hardcoded package vars in activities.go, zeroed in tests); Done set before the final activity.
- [Each workflow version is self-contained](references/workflow-code-organization.md) — v1/v2/v3 own their inline chain, state/getState, options & RegisterVN (registers only their activities); activities are exported funcs in activities.go with dwells hardcoded as package vars; only types.go contract + main.go switch shared.
- [Frontend conventions & gotchas](references/frontend-conventions.md) — no build (Tailwind CDN), HTMX-only, hypermedia URLs off `/api/`; traps: `@media` can't `@apply`, `#orders`/`#versions` must morph not replace, close modals with empty 200.
- [Commit message convention](references/commit-convention.md) — subjects start with an imperative action verb; no scope/type prefix.
- [Kubernetes namespace: pizza-tracker](references/k8s-namespace.md) — demo deploys to a dedicated `pizza-tracker` namespace owned by Kustomize; backend derives `TEMPORAL_DEPLOYMENT_NAME=$(POD_NAMESPACE)/pizza-worker` via the Downward API (same var the controller injects into the worker).
- [Distroless + runAsNonRoot needs numeric runAsUser](references/distroless-runasnonroot.md) — distroless `:nonroot` images have a non-numeric user, so the hardened pod securityContext must set `runAsUser: 65532` or pods hit CreateContainerConfigError.
- [Temporal host addresses: dev server vs kind cluster](references/temporal-host-addresses.md) — local dev server → `127.0.0.1:7233` (IPv4-only, never `localhost`); kind-cluster CLI → `temporal.127-0-0-1.nip.io:7233` (Traefik authority, else 404).
- [Verifying the rollout flow across deployment modes](references/verifying-rollout-across-modes.md) — modes share host :7233 so verify sequentially; drive via POST /deploy (stop 0/1/2), /rollback, /orders/{id}/recover; ground-truth via `deployment describe` JSON.
