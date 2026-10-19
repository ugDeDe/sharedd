# Review — Stage 08a blueprint (schema, registry, validator, storage, read/validate routes)

Read-only review of the stage-08a implementation against
[05](05-blueprint-profiles.md) / [04 §2](04-storage-and-execution.md) /
[03 §6](03-cabinet-api.md) and the stage contract
[implementation-stage-08a-blueprint.md](implementation-stage-08a-blueprint.md).

## Review Summary

| Severity | Count | Status |
|----------|-------|--------|
| CRITICAL | 0 | pass |
| HIGH     | 1 | block |
| MEDIUM   | 2 | info |
| LOW      | 5 | info |

**Verdict: BLOCK.**

One HIGH contract gap: `expected_version` CAS is required for graph `PUT`
(03 §6) but is absent and — because the envelope decoder is permissive — is
silently ignored instead of rejected. Everything else in scope conforms to the
spec bounded slice, the two authoritative templates validate with zero
diagnostics, no execution/run/ban-delivery surface leaked in, and management
profile mode is still 422.

Spot runs (all `ok`):

```
cd registry && go test -race -count=2 ./internal/blueprint ./internal/control \
  -run 'Blueprint|Validate|ProfileGraph|Template'   # ok
cd registry && go test -race -count=1 ./internal/server \
  -run 'Blueprint|Profile'                          # ok (23s)
```

---

## (a) Block registry vs 05 §4

**No drift found.** All 18 types match type-for-type: data in/out names and
types, control ports (`["ok"]` default; `policy.guard` = `active,stop`;
`ip.order|attach|switch_advertised` = `ok,fallback`), effect classes, retry
classes (`safe-read` only on `server.ready`, `pool.admit`,
`server.distinct_ip`, `server.absent`, `ip.verify`) and strict params
(`purpose`, `timeout_seconds`, `family=IP_V4`, `reason=policy_inactive`, else
`{}`). `registry_test.go` pins the table, control/retry/effects, provider caps
and timeout params.

- **Documented provider correction confirmed, not drift**:
  `server.distinct_ip` is valid for **both** providers (nodehost + beget)
  because the authoritative §7 Beget template uses it on the replacement path;
  `ip.*` blocks are beget-only. This matches impl-stage-08a §2 note and
  `TestProviderCapabilities`.
- **LOW — `observe.arm` control output.** 05 §4 header says the normal control
  output is `ok` "unless the table states otherwise"; the `observe.arm` row
  says "end run succeeded" but does not mark its control column "нет" (unlike
  `terminal.stop`, whose output is explicitly "нет"). The registry declares
  `Control: []string{}` for `observe.arm`, i.e. it is terminal. This is stricter
  than a literal reading and removes an allowed `ok` port. It does not affect
  either template (both arms are terminal). Either add `ok` to `observe.arm`
  or state the terminal semantics explicitly in 05 §4.

## (b) Validator vs 05 §5

All enumerated §5 checks are implemented and table-tested:

| §5 rule | Status | Evidence |
|---|---|---|
| schema/types/unknown keys | ok | `Parse` `DisallowUnknownFields` + trailing-junk reject; `bad_json` |
| allowlist + strict params | ok | `BlockFor` + `checkParams`/`checkParamValue` (`unknown_param`, `missing_param`, `bad_param_value`) |
| limits 128/256/256KiB | ok | `MaxNodes=128`, `MaxEdges=256` (control + node-output bindings, input./run. excluded correctly), `MaxBytes=256<<10`; also `MaxInputs/MaxNodeIn=32`, retry 1..5 / 1..300 |
| id pattern + reserved ids | ok | `idRe`; `reservedIDs{input,run}` on nodes |
| entries exist | ok | `entry_not_found`, entry ids via `idRe` |
| literal handle forbidden | ok | `literal_handle_forbidden` for config/server/operation/ssh_target/secretref |
| DAG control+data, no cycles | ok | `hasCycle` over combined edges |
| single-target fan-out | ok | `fan_out` per control `(from,port)` |
| reachability from both entries | ok | union of `reach(initial)`, `reach(observe)` |
| dangling ports/refs | ok | `unknown_control_port`, `dangling_ref`, `dangling_edge` |
| per-entry required inputs typed+reachable | ok | `entryRefsInput` per entry + global `checkRefType` |
| dominance / inactive-branch dep | ok | iterative dominators per entry; `inactive_branch_dep` |
| retry only on safe-read | ok | `retry_not_allowed` when `RetryClass != RetrySafeRead` |
| provider capability | ok | `provider_incompatible` |
| §5.5 static effect dominance | partial (documented) | replacement create ⊂ guard; fence ⊂ admit; switch ⊂ fence; delete ⊂ fence/switch; cleanup ⊂ admit; `cleanup_to_create`; `runtime_ref_on_initial` |
| §5.6 ownership/pinned/no plaintext/no URL | graph-local (documented) | handle literals forbidden; no URL representable |

Runtime-only items (CAS revision/owner-enabled, distinct provider IDs,
cross-object input ownership) are correctly deferred to stage-08b and flagged
in §7 of the stage doc.

## (c) Storage

**Conforms, with one coverage gap.**

- Additive + idempotent: `control.Open` runs `ensureColumn` per column behind
  `PRAGMA table_info` (control.go:358-370). `CREATE TABLE profile_versions`
  still omits the columns, so every fresh DB exercises the ALTER path.
- Existing text semantics unchanged: `format TEXT NOT NULL DEFAULT 'text'`,
  `blueprint_json DEFAULT ''`, `schema_version DEFAULT 0`, `content_hash
  DEFAULT ''`; `AddProfileVersion` still writes `playbook`/`vars_schema` and now
  reports `Format=FormatText`; `ProfileVersion` still serves text.
- Open-twice/three-times idempotency: `profile_graph_test.go` opens the same DB
  3× and asserts rows survive.
- Immutable graph version: `CreateProfileVersionGraph` = `max+1`, no update
  path, resets `published`, `playbook=''`. Numeric-only lookup
  (`ProfileVersionByID`/`AuthoredVersionFor`, `<=0 → ErrNotFound`).
- **LOW — no pre-existing legacy-row fixture.** The migration test creates the
  profile *after* `Open`, so it never inserts a text row into a
  columns-absent `profile_versions` and then opens. A legacy fixture (create
  old table without the 4 columns + insert a text row with `playbook`, then
  `Open`) would prove default backfill (`format='text'`, steps intact). Other
  tables already use this pattern (`TestOpenMigratesOperationsProfileID`).
- **HIGH — `expected_version` CAS missing (see F1).**

## (d) Routes

Blocks route, validate (zero side effects), numeric+ownership version
lookup (own **or** published; `latest`/foreign → 404), POST/PUT
`steps`+`blueprint` conflict → 400, and 422 diagnostics mapping all conform and
are tested. Text path is byte-compatible.

**F1 — HIGH: graph `PUT` has no `expected_version` CAS; the field is silently ignored.**
- Contract: 03 §6 — "PUT создаёт новую immutable версию, с `expected_version`
  (CAS, 409) для graph". Not listed in stage-doc §7 NOT-DONE.
- Evidence: `grep expected_version|ExpectedVersion` over `registry/internal`
  returns nothing. `profilePayload` (profiles_admin_api.go:65) has no
  `expected_version`; `handleMeUpdateProfile` (profiles_user_api.go:196)
  unconditionally calls `addGraphVersion`. Because `decodeCabinetJSONLimit`
  is not strict, a client that *sends* `expected_version` gets 200 + a new
  version, never 409 — the worst case (client believes CAS is honored).
- Impact: concurrent/stale graph edits silently pile up immutable versions with
  no conflict signal.
- Fix: add `ExpectedVersion *int json:"expected_version"` to `profilePayload`;
  in the graph branch of `handleMeUpdateProfile`, read the current max version
  and return `409 revision_conflict` when it differs, before appending.
- Acceptance test: create graph profile (version 1); `PUT` with
  `expected_version:1` → 200/new version 2; second `PUT` with stale
  `expected_version:1` → 409 and version count stays 2.

**F2 — MEDIUM: new-route envelopes are not strict-decoded.**
- Contract: 03 §2 — "строгий decode, unknown fields и второй JSON value
  запрещены".
- Evidence: `decodeCabinetJSONLimit` (cabinet_api.go:330) is plain
  `json.Decoder.Decode` — no `DisallowUnknownFields`, no trailing-value reject.
  Only the inner `blueprint` blob is strict (`blueprint.Parse`). This is what
  masks F1 and lets typos (`"step"`, `"blueprnt"`) fall through to the text
  path or be ignored.
- Fix: a strict helper (DisallowUnknownFields + EOF-after-first-value) used by
  `/api/me/profiles`, `/api/me/profiles/validate`, and `PUT`.
- Acceptance test: `POST /api/me/profiles` with an unknown envelope key →
  400 `validation_failed` (today: silently accepted).

**F3 — MEDIUM: graph `PUT` leaves a partial mutation on 422.**
- Evidence: `handleMeUpdateProfile` applies `RenameProfile` (line 223) *before*
  the graph branch validates (`addGraphVersion`, line 234). An invalid graph
  returns 422 but the name is already changed; `description` (applied after) is
  skipped. `POST` avoids this via `cleanup()`, `PUT` does not.
- Fix: validate the blueprint (or snapshot/restore the name) before any
  mutation; ideally reorder to validate → mutate.
- Acceptance test: `PUT` own profile with a valid new `name` + invalid
  `blueprint` → 422 **and** the stored name is unchanged.

**LOW — oversized body returns 400, not 413 `body_too_large`.** 03 §2 says a
too-large body → 413. `MaxBytesReader` failure is mapped by the handlers to
`400 validation_failed`. Also makes the `too_large` diagnostic effectively
unreachable over HTTP (envelope cap fires first). Low impact; map
`*http.MaxBytesError` → 413.

## (e) §6/§7 templates

Both `testdata/nodehost_replacement.json` and
`testdata/beget_ip_or_replacement.json` are verbatim copies of 05 §6/§7 and
`TestTemplatesValidate` asserts **0 diagnostics** and the expected provider.
No spec/implementation mismatch. (Also exercised end-to-end through
`/api/me/profiles/validate` and `/api/me/profiles` in
`blueprint_api_test.go`.)

## (f) Leak / scope check

- No execution/run/ban-delivery surface: `grep profile_runs|ban_deliver|
  profile-runs|expected_version` over `registry/internal` → none.
- Management profile mode still `422 profile_incompatible`
  (`cabinet_cloud.go:1044`, asserted in `cabinet_cloud_test.go`).
- No new dependencies; node/sshx/provision untouched (per stage doc §1).

## Other LOW observations

- `MaxRetryBackof` is a typo for `MaxRetryBackoff` (model.go:21) — exported
  identifier, worth fixing before it spreads.
- `CanonicalBytes` is only canonical for map keys and struct fields;
  `json.RawMessage` literals (`Binding.Literal`, `Node.Params`) are compacted
  but not re-normalised, so e.g. literal `1` vs `1.0` hash differently. In
  practice params/literals are scalars with ≤1 key, so impact is negligible;
  note it if `content_hash` is ever used for dedup.

## Fix ordering

1. F1 (HIGH) + F2 (strict decode) together — the decoder fix makes F1 loud.
2. F3 (PUT ordering).
3. LOW items as cleanup (413 mapping, `observe.arm` conformance, legacy
   fixture, typo).

No application edits were made; this document is the only file written.
