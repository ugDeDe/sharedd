# Blueprint editor — fast UI contract/security review

**Verdict: BLOCKED — one High paid-action safety finding.**
**Verification: partially_verified (source/contract inspection only).**

## Scope and boundary

Read `implementation-blueprint-editor.md`, `design-blueprint-editor.md`, the
seven `ui/pages/cabinet/blueprint-*.ts` modules, `api.ts`, relevant `main.ts` /
`node-detail.ts` integrations, shared escaping/vars helpers, and actual backend
blueprint/version/run/management/profile handlers and graph registry/schema.
Actual handlers, not earlier stage descriptions, were treated as authoritative.
This is not a re-review of prior backend stages or a general backend audit.

No application edits, browser operations, tests, vet, builds, commands, commits,
memory writes or delegation were performed. The designer's parallel mocked-preview
QA is separate and is not claimed here. Only this report was written.

## Prioritized blocker

### H1 — A rejected replay clears a previously ambiguous paid-run intent

- **Severity/category:** High / paid-action safety, idempotency.
- **Location:** `ui/pages/cabinet/blueprint-runs.ts:118–129`; same-key replay at
  `:143–145`, fresh-key submission at `:146–174`.
- **Concrete source-traced sequence:**
  1. Explicitly submit a run with optional `node_id` left blank. The server accepts
     it, but its response is lost. The UI correctly retains the original body/key
     in `uncertainRun` and disables ordinary submission.
  2. Select “Повторить тот же запрос с тем же ключом” after the CSRF cookie has
     expired/disappeared. `api.ts:387–391` rejects locally with status 403; the
     original run has not been reconciled. A server-side `csrf_invalid` response
     has the same effect (`api.ts:403–404`).
  3. `send()` determines ambiguity from **only this latest error**. Since 403 is
     not ambiguous by that test, it sets `uncertainRun = null`, hides same-key
     replay and enables the normal fields/submit button. Reopening the review
     also sees no unresolved intent.
  4. Once CSRF access is restored, ordinary submission allocates a new key for the
     same request. The first accepted run may still be purchasing/executing.
- **Why this can duplicate paid work:** a rejected replay does not establish that
  the original request failed. The backend deduplicates creation using the
  user-scoped key (`blueprint_runs_api.go:151–155,305–332`); a new key is a new
  intent. Leaving `node_id` blank supplies no named-deployment identity on which
  to rely for the separate unresolved-deployment guard. CSRF rejection occurs
  before the handler's stored acknowledgement can be replayed
  (`blueprint_runs_api.go:90–94`).
- **Required bounded correction:** distinguish a definitive rejection of a first
  submission from failure to reconcile an already-ambiguous submission. Once an
  intent is uncertain, preserve its exact body/key and keep fresh-key submission
  disabled across **all** unsuccessful replay outcomes, including local/server
  403, other 4xx and `idempotency_conflict`. Clear it only on its matching successful
  acknowledgement or a deliberately designed reconciliation that proves its
  disposition. Do not retry automatically or change backend behavior.
- **Acceptance trace (not executed):** accepted-but-response-lost → same-key replay
  rejected with 403 → close/reopen review → restore CSRF. The original body/key
  must remain the only available mutation intent; no new UUID may be generated.
  A definitive initial 4xx with no preceding uncertainty may still permit editing.

## Other requested boundaries checked

| Area | Source-level assessment |
|---|---|
| Version ordinal versus version PK | Numeric readers use `/versions/{ordinal}`; saves send the loaded ordinal as `expected_version`; run/policy bodies use the resolved `profile_version_id`. Run graph display compares pinned PK. |
| Graph versus text installation/copy | `openVisibleProfile` branches on resolved format; graph copies become local graph drafts. Legacy install selectors use `textProfiles()` and exclude unresolved/graph versions. No graph use of the legacy text-copy endpoint found. Published/default exclusion is an explicitly documented capability gate, not a new backend defect. |
| Unexpected paid POST | Save/template/validate/navigation do not create runs. Initial run POST follows explicit consent and instance validation; Profile mode PUT follows separate consent. H1 is the exception in preserving an already-authorized uncertain intent, not an automatic POST. |
| Cancel/resume ambiguity | Network/5xx ambiguity locks commands for that run in the current session; no automatic new-key command retry found. Displayed revision is carried into resume. Safe retry is restricted to a resolved read/safe-read block; purchases/SSH are not offered as retry-safe. |
| HTML/XSS | Shared `esc` escapes `&`, `<`, `>`, both quotes. Reviewed user-controlled names, descriptions, graph labels/bindings, diagnostics and journal errors use escaping or `textContent`; no concrete new raw-HTML injection found. |
| Credentials/vars | No new credential input, graph/vars browser-storage persistence, URL serialization or console logging found in the reviewed blueprint paths. Run vars remain request/in-memory data; checkpoint rendering allowlists handle identifiers rather than dumping arbitrary output. |
| Beget safety | `runtimeGate` blocks non-Nodehost and `ip.*` execution; run entry checks it and policy collection checks it again. Beget remains author/validate/save only, not executable merely because registry capabilities include it. |
| Typed block registry | Frontend port arrays, nullable params schema, effect/retry classes and capabilities match `internal/blueprint/registry.go`; inspector/control connections use returned names, not invented target/data ports. |
| Dirty navigation and strict schema | Hash navigation/logout call the dirty-leave guard; unload warns. `Draft.positions`, pan and zoom are separate from the blueprint. Authoring sends only graph/envelope fields accepted by the strict backend; layout coordinates are intentionally local-only, not supposed to survive server reload. |
| Management activation claims | Profile activation has a distinct permission checkbox and current-ban opt-in; Manual copy does not promise automatic purchases. The UI does not claim `observe.arm` creates a policy, a 200/202 means execution completion, or an absent `active_run_id` means no run exists. |

No additional concrete Critical/High finding was identified in this bounded read.
These observations are source-level only, not browser/WCAG/runtime certification.

## Release/re-review gate

Do not mark the editor's paid-run flow PASS until H1 is corrected and the focused
failure sequence is re-reviewed. No fix or re-review was performed in this task.
Tests, type checks, vet/builds and live/provider behavior were deliberately not
verified, as requested; earlier implementation claims were not rerun.
