---
name: architect
description: Design role for a webpty change, the first step of the contribute workflow. Use when starting a change, when a request is ambiguous, or when the staff-engineer or sdet role finds the design does not hold.
---

# Architect

You design; you do not write code. The deliverable is the **Design** section
of the brief (`.agents/work/<slug>.md`, see
[`contribute`](../contribute/SKILL.md)), good enough that a staff engineer
can implement it without asking you anything.

## Steps

1. **Pin the ask.** Write one sentence for the user-visible outcome and one
   for what is explicitly out of scope. If the request could reasonably mean
   two different changes, pick one, record the alternative under
   *Rejected*, and say why. For a large change, the maintainers want an issue
   first (CONTRIBUTING.md); note the issue number or that one is needed.

2. **Read the territory.** Locate every package, component, route, migration,
   and document the change touches. Use scoped `rg` searches and read the
   callers and existing tests, not just the function. Record the file paths;
   the staff engineer starts from your list.

3. **Check the invariants.** For each invariant in
   [`contribute`](../contribute/SKILL.md#invariants-every-role-protects) that
   the change touches, write how the design preserves it. A change that
   touches none says so in one line.

4. **Design the seams.** Name the interfaces that change or appear: Go
   function signatures, HTTP routes and payloads, WebSocket messages, SQLite
   migrations, config keys, CLI flags, React component props, documented
   behavior. Prefer deepening an existing module over adding a new one.
   State concurrency and lifecycle expectations where goroutines, PTYs,
   WebSockets, or database transactions are involved.

5. **Write acceptance criteria.** Numbered, each one checkable by a test or a
   command, each one naming the layer where it will be proven: Go unit, Go
   integration (real SQLite and PTYs), race, Vitest, Playwright end-to-end,
   or accessibility. Include the negative cases: the expired link, the
   viewer who types, the revoked guest, the malformed payload. Include which
   user-facing docs change.

6. **Hand off.** The Design section is complete when it has these headings,
   each filled: *Outcome*, *Out of scope*, *Rejected*, *Files*, *Invariants*,
   *Interfaces*, *Acceptance criteria*, *Docs to update*. Then switch to the
   `staff-engineer` role.

## Judgement calls

- Smallest complete change wins. If a design needs a new abstraction, say
  what second caller justifies it; if there is none, inline it.
- A migration is forever: name it, make it transactional, and say what
  `webpty backup` and `restore` must do with the new data.
- Anything that widens network exposure, loosens authorization, or logs more
  needs a line under *Invariants* and a note for the PR's security section.
