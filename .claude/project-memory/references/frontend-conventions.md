---
name: "Frontend conventions and gotchas"
description: "Non-derivable frontend directives and traps: no build / Tailwind Play CDN, zero JS (HTMX only), hypermedia URLs (never /api/), the @media-can't-@apply trap, morph-not-replace, 200-not-204, no per-version failure count"
type: feedback
---

# Frontend conventions and gotchas

Only the non-obvious rules and traps are kept here — the actual markup/CSS lives
in `frontend/index.html` and the Go templates and is self-describing.

**Directives (intent, not visible from the code):**

- **No build step.** Tailwind comes via the Play CDN (`@tailwindcss/browser@4`),
  which only processes `<style type="text/tailwindcss">`, so all styling lives in
  the single `<style>` block in `index.html`. Do **not** add a Node/PostCSS build
  or an external `.css`; if one is truly needed, surface the trade-off first.
- **Zero application JavaScript — all interactivity is HTMX.** SSE pushes
  server-rendered HTML (not JSON). No `<script>` block, no
  `onclick`/`oninput`/`hx-on` (only the head's CDN `<script src>` includes). Wire
  new interactions with htmx attributes + small fragment endpoints, never JS.
- **Hypermedia URLs, never `/api/`.** The HTMX endpoints return server-rendered
  HTML fragments, not JSON, so per HTMX's own guidance they deliberately avoid
  the `/api/` prefix (which signals a stable JSON *data* API) and are named after
  the resource + UI need: `GET`/`POST /deploy`, `GET /deploy/ramp`,
  `GET`/`POST /rollback`, `POST /orders/{id}/recover`, the modal is closed
  with `DELETE /modal` (a resource delete, not a generic `/close` verb), and
  order publishing is paused with `PUT /pause` and resumed with `DELETE /pause`.
  `GET /events` (SSE), `GET /healthz` and `/` (the SPA) are unchanged. When
  adding an endpoint, follow this scheme — do **not** reintroduce `/api/`.
- **No per-version failure count.** Version cards show only `N in flight`. Every
  activity retries forever (see [[demo-timing]]), so no workflow ends Failed and a
  failure count would mislead; the retrying state is shown per order (red card).
  Recover is per-card (resets one workflow onto Current — see
  [[worker-versioning-model]]), never a global control.

**Traps (each cost real debugging time):**

- **`@media`-based variants do NOT compile via `@apply` in the Play CDN.**
  `@apply max-[760px]:…` and `prefers-reduced-motion:…` emit *nothing* — keep
  responsive and reduced-motion rules as raw `@media` blocks. Non-media
  selectors (`:hover`, `:has`, `:not`), arbitrary-property utilities and
  `animate-*` all compile fine via `@apply`.
- **`#orders` must morph, not replace.** It uses idiomorph (`morph:innerHTML`)
  with stable `id="order-{ID}"` cards; a plain `innerHTML` swap recreates nodes
  every tick and kills the entry animation and the CSS stepper-fill transition.
  The Done card's visible-then-collapse is pure CSS, sized to finish within
  `DeliveredDwell` (see [[demo-timing]]).
- **`#versions` also morphs** for the same reason. The Deployment-zone cards
  carry stable `id="ver-{Version}"` and the traffic-bar fill stable
  `id="bar-{Version}"`, so the bar's `width` (`--bar-w`) transition glides when
  the ramp % changes instead of jumping. A plain `innerHTML` swap (the old
  default) recreated the cards every tick and killed every transition.
- **The status chip's `id` is content-keyed ON PURPOSE.** It is
  `id="chip-{Version}-{Status}-{TrafficPct}"`, so when status or % changes the id
  changes, idiomorph drops + re-inserts the node, and that re-insertion replays
  the `chip-pulse` CSS entry animation (zero-JS pulse on every deployment change).
  Do **not** "simplify" it to a stable id — a stable id morphs in place and the
  pulse never fires. The `.pin` "N in flight" count is deliberately left
  unanimated (it changes nearly every tick; pulsing it is noise).
- **HTMX won't swap on 204.** Modal-close / fragment-clear endpoints must return
  an **empty 200**, not 204, or the modal stays open.

The theme follows [temporal.io/brand](https://temporal.io/brand): UV `#444ce7` is
the accent and v1, v2 = green, v3 = amber (amber also = ramping).
