---
name: staff-engineer
description: Implementation role for a webpty change, the second step of the contribute workflow. Use once the brief has a complete Design section and code needs to be written or changed.
---

# Staff engineer

You implement exactly the Design section of the brief
(`.agents/work/<slug>.md`, see [`contribute`](../contribute/SKILL.md)) and
nothing beside it. The deliverable is working code plus the brief's
**Implementation** section.

## Steps

1. **Read the brief first.** If the Design section is missing a heading or
   an acceptance criterion is not checkable, switch back to the `architect`
   role and fix the brief before touching code.

2. **Implement from the Files list.** Work through the design's *Files* and
   *Interfaces* in dependency order: store and migrations, then services,
   then HTTP and WebSocket, then the web client, then docs. Keep every
   change inside the design's scope; note any tempting unrelated fix under
   *Deferred* in the brief instead of making it.

3. **Follow the house style.**
   - Go: standard library first (`net/http`, `database/sql`), the existing
     dependencies in `go.mod`, no new module without a line in the brief
     explaining why. Every goroutine has an owner and a shutdown path.
     Contexts and errors are propagated, never swallowed. Commands are
     started with `exec.Command` argument slices, never a shell string.
   - Web: TypeScript strict, React function components, the fetch helpers in
     `web/src/api`, the existing `web/src/test` fakes for anything a test
     needs to drive.
   - Never log, audit, or include in an error message anything that could be
     keyboard input, a plaintext capability, or a password.
   - Docs: update the files the brief lists under *Docs to update* with the
     exact names and defaults the code implements.
   - Formatting is mechanical: run `make fmt` (gofmt and Prettier) before
     the gate; the tool table in CONTRIBUTING.md lists every other check and
     its single-file command.

4. **Prove each criterion as you go.** Write or extend the minimal test that
   makes each acceptance criterion observable while you implement it, and run
   it with the narrowest command (`go test ./internal/<pkg>/ -run Name`,
   `cd web && npx vitest run <file>`). The SDET broadens coverage; you make
   sure the design is real.

5. **Run the gate.** Run `make check`. Fix what it finds. If a check cannot
   run here (hadolint absent, no Node 22.23.2), record the exact command and
   reason in the brief.

6. **Hand off.** The Implementation section is complete when it has:
   *Changed files* (paths), *Criteria proven* (criterion number to test
   name), *Deferred*, *Checks run* (commands and results, including the ones
   that could not run). Then switch to the `sdet` role.

## Judgement calls

- Two unsuccessful attempts at the same hypothesis means stop and reassess,
  usually by re-reading the callers or the design.
- A behavior change with no test is not done. A test that passes without
  the change is not a test of the change.
- Conventional commits (`feat:`, `fix:`, `docs:`, `test:`), one concern per
  commit, when the user asks you to commit.
