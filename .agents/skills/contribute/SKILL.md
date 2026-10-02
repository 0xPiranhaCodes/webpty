---
name: contribute
description: Use before changing webpty code or behavior, whether a feature, bug fix, refactor, or dependency change. Routes the work through three roles in order, architect to staff-engineer to sdet, each with its own skill and hand-off.
---

# Contributing to webpty as three roles

Every change passes through three roles, in this order, and each role leaves
its work in one shared **brief** that the next role reads first:

| Order | Role | Skill | Produces |
| ----- | ---- | ----- | -------- |
| 1 | Architect | [`architect`](../architect/SKILL.md) | Design: scope, constraints, interfaces, acceptance criteria |
| 2 | Staff engineer | [`staff-engineer`](../staff-engineer/SKILL.md) | The smallest complete implementation of the design |
| 3 | SDET | [`sdet`](../sdet/SKILL.md) | Tests for every acceptance criterion, full validation, evidence |

Take the roles one at a time. Finish a role's completion criterion before
starting the next, and switch roles explicitly: say which role you are now in.
A later role that discovers the design is wrong returns to the architect role
and revises the brief rather than improvising.

## The brief

The brief is `.agents/work/<slug>.md`, where `<slug>` is the conventional
commit subject in kebab case (`reject-expired-share-links`). It is git-ignored
and lives for one change. Create it in the architect role; append to it in the
later roles; keep its sections in this order:

```markdown
# <conventional commit subject>

## Design          (architect)
## Implementation  (staff-engineer)
## Validation      (sdet)
```

The pull request is written from the brief: the PR template's sections map
onto it one to one.

## Invariants every role protects

These come from [docs/specs/product.md](../../../docs/specs/product.md) and
[CONTRIBUTING.md](../../../CONTRIBUTING.md); the brief must say how the change
keeps each one that it touches:

- Secure defaults: loopback bind, forced first-run password change, commands
  never run through a shell, terminals get only an allowlisted environment.
- Keystrokes and input payloads never enter recordings, logs, audit details,
  analytics, or error messages.
- Capabilities are stored only as hashes; the plaintext appears once, at creation.
- Every API request and WebSocket is authorized on the server; revocation
  disconnects active guests.
- One concern per pull request. Unrelated refactoring is a separate change.

## Trivial changes

A docs-only, comment-only, or single-line fix with an existing test still
takes all three roles; the architect and SDET sections may each be two lines.
Skipping a role is never the shortcut; shrinking its section is.
