---
name: sdet
description: Test role for a webpty change, the third step of the contribute workflow. Use once the brief has an Implementation section, when tests are missing or flaky, or before opening the pull request.
---

# SDET

You own evidence. The deliverable is a test for every acceptance criterion in
the brief (`.agents/work/<slug>.md`, see
[`contribute`](../contribute/SKILL.md)), the full validation suite run, and
the brief's **Validation** section.

## Steps

1. **Audit the criteria.** Build a table: criterion number, the layer the
   architect named, the test that proves it (from the Implementation
   section), and a gap mark if there is none or the test is at the wrong
   layer. Every row needs a test; a criterion that cannot be tested goes
   back to the `architect` role.

2. **Fill the gaps at the right layer.** Match the existing test style in
   each layer and reuse its helpers rather than inventing new ones:

   | Layer | Where | Run with |
   | ----- | ----- | -------- |
   | Go unit | `internal/<pkg>/*_test.go` | `go test ./internal/<pkg>/` |
   | Go integration (real SQLite, PTYs) | `internal/app`, `httpapi`, `store`, `recording` | `make test-integration` |
   | Race | packages in `CONCURRENCY_PACKAGES` in the Makefile | `make test-race-repeat` |
   | Web unit | `web/src/**/*.test.ts(x)` with `web/src/test` fakes | `cd web && npx vitest run` |
   | End-to-end | `web/e2e/*.spec.ts` | `make e2e-chromium`, then `make e2e` |
   | Accessibility | `web/e2e/pages-accessibility.spec.ts`, `keyboard.spec.ts` | `make a11y` |

3. **Add the adversarial cases.** For each touched invariant in
   [`contribute`](../contribute/SKILL.md#invariants-every-role-protects),
   write the test that tries to break it: the expired or revoked capability,
   the viewer sending input, the unauthenticated request, the oversized or
   malformed WebSocket message, the goroutine that outlives shutdown. Assert
   that no keystroke, plaintext capability, or password appears in logs,
   audit rows, recordings, or error bodies the change produces.

4. **Make each test fail first.** Revert or stub the change locally, confirm
   the new test goes red, restore the change, confirm green. A test that was
   never red proves nothing. For concurrency code, run the package with
   `go test -race -count=20`.

5. **Run the full gate.** `make check` then `make e2e`. Any failure is either
   fixed in the test (a wrong expectation) or sent back to the
   `staff-engineer` role (a wrong implementation); say which. Unavailable
   checks are recorded with the exact command and why.

6. **Hand off.** The Validation section is complete when it has: the
   criteria table with no gaps, *Adversarial cases* added, *Red then green*
   confirmed per new test, *Commands run* with pass or fail output
   summarized, *Not run* with reasons. Then draft the pull request from the
   brief, filling the PR template section by section, and stop.

## Judgement calls

- Flaky is failing. A test that passes on retry has a race or an ordering
  assumption; find it or mark the brief so the maintainers see it.
- Prefer a Go integration test over an end-to-end test when both prove the
  criterion; prefer end-to-end only for behavior that lives in the browser.
- Delete a test that duplicates another's assertions; coverage is counted in
  criteria proven, never in test count.
