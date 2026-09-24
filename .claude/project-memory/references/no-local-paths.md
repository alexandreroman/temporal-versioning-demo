---
name: "No local machine paths in the repo"
description: "Repo files (code, comments, docs, project memory) never reference local file paths or sibling local projects"
type: feedback
---

# No local machine paths in the repo

Nothing committed to the repo — source, comments, templates, tests, docs, and
this project memory — references paths or projects from a developer's machine
(`/Users/...`, `~/Projects/...`, names of sibling local checkouts). Design
inspiration and conventions are described on their own terms (e.g. "a
temporal.io-inspired violet space theme").

**Why:** the repo is public and shared with the team; local paths are
meaningless to others and leak personal machine layout.

**How to apply:** when a task points at a local file for inspiration, read it,
then describe the resulting convention without citing its location. Brief
subagents with the same rule. Relates to [[frontend-conventions]].
