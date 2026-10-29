# Stage 08a — Blueprint schema, registry, validator, storage, read/validate routes

Bounded slice of [05](05-blueprint-profiles.md) / [04 §2](04-storage-and-execution.md) /
[03 §6](03-cabinet-api.md): the graph profile **format**, its **allowlisted
block registry**, **server-side validation**, **immutable version storage** and
the **read/validate/profile routes**. No execution engine, no runs, no ban
delivery, no management profile mode, no UI. Those remain stage-08b / later.

## Status

**Implemented and verified** for the bounded scope. `go vet`, `go test -race`
(`internal/blueprint`, `internal/control`, `internal/server`), full
`go test ./...`, `go build -mod=readonly ./...`, `gofmt -l .` (empty) and
`git diff --check` are clean.

## 1. Files

New package `registry/internal/blueprint/`:

| File | Contents |
|---|---|
| `model.go` | `Blueprint`, `Entries`, `Input`, `Node`, `Binding`, `Retry`, `Edge`, `Diagnostic`, type/effect/retry/provider constants, hard limits. |
| `registry.go` | `Block`, `Port`, `Param`, `BlockPorts`; the 18-block §4 table; `Blocks()`, `BlockFor()`, `IsDatatype()`. |
| `validate.go` | `Parse`, `ValidateBytes`, `Validate`, `ValidateInputs`, `CanonicalBytes`, `CanonicalHash`; structural + semantic checks. |
| `model_test.go`, `registry_test.go`, `validate_test.go` | Table-driven tests incl. both §6/§7 templates. |
| `testdata/nodehost_replacement.json`, `testdata/beget_ip_or_replacement.json` | The two spec templates, verbatim. |

Modified (additive only):

| File | Change |
|---|---|
| `registry/internal/control/control.go` | Idempotent `ensureColumn` migration for 4 `profile_versions` columns. |
| `registry/internal/control/profiles.go` | `ProfileVersion` extended (Format/Blueprint/SchemaVersion/ContentHash); `CreateProfileVersionGraph`, `ProfileVersionByID`, `AuthoredVersionFor`; `AddProfileVersion` now returns `Format="text"`. Existing text methods unchanged. |
| `registry/internal/control/profile_graph_test.go` | New storage/migration test. |
| `registry/internal/server/blueprint_api.go` | New routes + DTOs + diagnostic 422; strict envelope decoder `decodeGraphJSONStrictLimit`; split `prepareGraphVersion`/`appendGraphVersion`/`currentGraphVersion`. |
| `registry/internal/server/blueprint_api_test.go` | New route tests incl. CAS, strict decode, no-partial-mutation, text-PUT. |
| `registry/internal/server/profiles_user_api.go` | Mount new routes; graph CAS + validate-before-mutate ordering in PUT; strict decode on POST/PUT. Text path byte-compatible. |
| `registry/internal/server/profiles_admin_api.go` | `profilePayload` gains `format`, `blueprint` and `expected_version`. Admin handlers otherwise untouched (see NOT-DONE). |
| `registry/internal/server/cabinet_api.go` | `decodeCabinetJSONLimit` helper (256KiB graph bodies); unchanged by review fixes (strict variant lives in `blueprint_api.go`). |

No new dependencies. `node/`, `sshx`, `provision`, hosting/cloud routes and
earlier stage files untouched.

## 2. Block registry as implemented (05 §4)

Control ports default to `["ok"]` unless listed. Effect / retry classes per §4.

| type | data in | data out | control ports | effect | retry | providers |
|---|---|---|---|---|---|---|
| `server.create` | connection:number, configuration:config | operation:operation | ok | purchase | none | nodehost, beget |
| `server.ready` | operation:operation | server:server | ok | read | safe-read | nodehost, beget |
| `install.text` | server:server, version:number | server:server | ok | mutation | none | nodehost, beget |
| `pool.admit` | server:server | server:server | ok | read | safe-read | nodehost, beget |
| `event.ip_ban` | server:server | server:server, episode:string | ok | read | none | nodehost, beget |
| `policy.guard` | server:server, episode:string | server:server, episode:string | active, stop | read | none | nodehost, beget |
| `server.distinct_ip` | old:server, new:server | server:server | ok | read | safe-read | nodehost, beget |
| `resource.fence` | old:server, new:server | server:server | ok | mutation | none | nodehost, beget |
| `resource.switch` | old:server, new:server | server:server | ok | mutation | none | nodehost, beget |
| `server.delete` | server:server | server:server | ok | mutation | none | nodehost, beget |
| `server.absent` | server:server | server:server | ok | read | safe-read | nodehost, beget |
| `observe.arm` | server:server | — | — | mutation | none | nodehost, beget |
| `ip.order` | server:server | server:server | ok, fallback | purchase | none | beget |
| `ip.attach` | server:server | server:server | ok, fallback | mutation | none | beget |
| `ip.switch_advertised` | server:server | server:server | ok, fallback | mutation | none | beget |
| `ip.cleanup` | server:server | server:server | ok | mutation | none | beget |
| `ip.verify` | server:server | server:server | ok | read | safe-read | beget |
| `terminal.stop` | — | — | — | control | none | nodehost, beget |

Strict params (`additionalProperties:false`, all required):

| type | params |
|---|---|
| `server.create` | `purpose` ∈ {initial, replacement} |
| `server.ready`, `install.text`, `pool.admit`, `server.absent`, `ip.switch_advertised` | `timeout_seconds` (integer ≥1) |
| `ip.order` | `family` ∈ {IP_V4} |
| `terminal.stop` | `reason` ∈ {policy_inactive} |
| all others | `{}` (any key rejected) |

> Note: `server.distinct_ip` is declared valid for **both** providers because
> the §7 Beget template uses it on the replacement path. Making it
> nodehost-only would have failed the authoritative Beget template; the spec
> wins over an assumed capability split. `ip.*` blocks are beget-only.
> `timeout_seconds` has a documented upper bound of 86400 in code (single
> integer bound; split per block if provider limits diverge).

## 3. Validation rules implemented vs 05 §5

| §5 rule | Status | Notes |
|---|---|---|
| 1. size/schema/types/unknown keys, allowlist + strict params, retry/wait bounds | **full** | `Parse` (DisallowUnknownFields + trailing-junk reject), `ValidateBytes` 256KiB cap, per-`type` param check. |
| 1. provider capabilities | **full** | block `capabilities` must contain `blueprint.provider`. |
| 1. both entries exist | **full** | `entry_not_found`. |
| 2. DAG over control **and** data deps, no cycles | **full** | combined-graph DFS. |
| 2. no parallel fan-out (one edge per output) | **full** | `fan_out`. |
| 2. no unreachable nodes | **full** | union of both entry reaches. |
| 2. dangling ports / refs | **full** | `dangling_edge`, `dangling_ref`, `unknown_control_port`. |
| 3. per-entry required inputs sourced + typed | **full** | `missing_required_input`, `type_mismatch`, `unknown_input_ref`. |
| 3. dominance of node-output on every path (inactive branch) | **full** | iterative dominators per entry; `inactive_branch_dep`. |
| 4. merge needs explicit server output; no implicit data merge | **partial** | enforced structurally: every declared data input must be bound exactly once; no data-output merging concept exists in v1. |
| 5. guard / admit / fence / switch / provenance dominance | **partial (static subset)** | checked: replacement `server.create` dominated by `policy.guard`; `resource.fence` dominated by `pool.admit`; `resource.switch` dominated by `resource.fence`; `server.delete` dominated by fence/switch; `ip.cleanup` dominated by `pool.admit`; no control path cleanup→create; `run.current_server` forbidden on initial. Not statically expressed: CAS policy revision/owner-enabled checks and “different provider IDs” (runtime). |
| 6. ownership of refs, pinned versions, no plaintext secrets / raw HTTP URL | **full (graph-local)** | literal `config/server/operation/ssh_target/secretref` forbidden (`literal_handle_forbidden`); refs must resolve to declared outputs; no URL literal is representable (only typed values). Cross-object ownership of `input.connection_id` etc. is a run-time check. |

Diagnostics: `[]Diagnostic{NodeID, Port, Code, Message}` (05 §5, 03 §6).

`CanonicalHash` = sha256 hex over canonical JSON (sorted keys, compact,
`json.Number`-preserving). `CreateProfileVersionGraph` stores exactly the
canonical bytes hashed.

## 4. Storage migration (04 §2)

`profile_versions` extended additively in `control.Open` via guarded
`ensureColumn` (checks `PRAGMA table_info`; idempotent on repeat open):

```
format         TEXT    NOT NULL DEFAULT 'text'
blueprint_json TEXT    NOT NULL DEFAULT ''
schema_version INTEGER NOT NULL DEFAULT 0
content_hash   TEXT    NOT NULL DEFAULT ''
```

Existing rows keep `format='text'` and their `playbook`/`vars_schema` exactly;
no semantics change. New API:

- `CreateProfileVersionGraph(profileID, blueprintJSON, schemaVersion, contentHash)` — immutable version `max+1`, `format='graph'`, resets `published` like the text path, `playbook=''`.
- `ProfileVersionByID(profileID, version)` / `AuthoredVersionFor(...)` — numeric version only (`<=0` → `ErrNotFound`), returning format + blueprint + steps + vars_schema.
- Text methods (`AddProfileVersion`, `ProfileVersion`, `CopyProfile`, admin CRUD) unchanged; `AddProfileVersion` now also reports `Format="text"`.

Migration idempotency verified by opening the same DB three times and by the
full existing control suite.

## 5. Routes (03 §6)

| Route | Auth | Behaviour |
|---|---|---|
| `GET /api/me/blueprint-blocks` | session | `200 {schema_version:1, blocks:[{type,ports:{inputs,outputs,control},params_schema,effects,retry_class,capabilities}]}` |
| `POST /api/me/profiles/validate` | session + CSRF | `{format:"graph",blueprint,inputs?}` → `200 {valid,diagnostics[]}`; strictly no storage/execution. |
| `GET /api/me/profiles/{id}/versions/{version}` | session | numeric version (not `latest`); own **or** published profile, else 404 → `{profile_id,version,profile_version_id,format,steps?,blueprint?,vars_schema}`. |
| `POST /api/me/profiles` | session + CSRF | accepts `{format:"graph",blueprint,...}` → creates profile + immutable graph version (201). Text body unchanged. |
| `PUT /api/me/profiles/{id}` | session + CSRF | same graph branch; `steps` + `blueprint` together → 400 `steps_and_blueprint_conflict`; invalid blueprint → 422 `invalid_blueprint` + diagnostics. |

Graph bodies capped at 256KiB; text `steps` still capped at 64KiB. Ownership is
enforced on the profile id before reading a version.

## 6. Test matrix and results

`registry/internal/blueprint` (all `ok`):

| Area | Cases |
|---|---|
| model | unknown top-level/node/binding field, trailing JSON rejected; minimal accepted; canonical hash stable across map order; binding one-of. |
| registry | 18 blocks sorted; control ports/effects/retry for guard/ip.*/ready/install/arm/stop/create; provider capabilities; timeout params. |
| structural | unknown block/param, missing/unknown param value, 129 nodes, >256 edges, >256KiB, wrong literal type, literal handle forbidden, binding both, cycle, fan-out, unreachable, dangling ref, missing entry, retry on mutation, retry bounds, reserved id, duplicate node, provider mismatch. |
| semantic | replacement without guard, delete without fence, cleanup without admit, runtime ref on initial, missing required input, inactive-branch dependency. |
| templates | **both §6 Nodehost and §7 Beget templates validate for their providers (0 diagnostics)** — no spec/implementation mismatch found. |
| inputs | `ValidateInputs` type/required/unknown. |

`registry/internal/control`: migration idempotent across reopen; text vs graph
version coexistence; numeric lookup; published reset.

`registry/internal/server`: blocks payload (18); validate valid + invalid with
zero side effects; graph create → version DTO; `latest` → 404; foreign
unpublished version → 404; steps+blueprint conflict → 400; invalid → 422 with
no leftover profile; text create + version route still `format:"text"`.

Verification commands and results (from `registry/`):

```
go vet ./...                         # clean
go test -race ./internal/blueprint ./internal/control ./internal/server -count=1   # ok
go test ./... -count=1               # ok (all packages)
go build -mod=readonly ./...         # ok
gofmt -l .                           # empty
git diff --check                     # clean
```

## 7. NOT DONE (out of scope)

- Execution engine, `profile_runs`, checkpoints, scheduler, `POST /api/me/profile-runs` (stage-08b).
- Ban delivery / `ban_deliveries` / reconciler.
- Management policy `PUT /api/me/nodes/{id}/management` profile mode — still 422 (`profile_incompatible`/not enabled).
- Admin panel graph authoring/publish: admin routes remain **text-only**; a graph version is only creatable via `/api/me/profiles`. Publishing a graph through admin is not wired yet.
- UI graph editor (05 §1).
- Runtime policy-revision / ownership / provider-ID precondition enforcement and cross-object input ownership (run-time, stage-08b).

## 8. Review fixes (against review-stage-08a-blueprint.md)

All findings from the read-only review that were in scope are fixed; the review
file itself is unchanged.

| Finding | Sev | Fix |
|---|---|---|
| F1 `expected_version` CAS missing | HIGH | `profilePayload.ExpectedVersion *int json:"expected_version"`. Graph `PUT` compares it to the current max version *before* any mutation; mismatch → `409 revision_conflict`. Absent → legacy text callers unchanged. `currentGraphVersion` treats a version-less profile as 0. |
| F2 permissive envelope decode | MEDIUM | `decodeGraphJSONStrictLimit` (`DisallowUnknownFields` + reject any trailing JSON value) applied to `POST /api/me/profiles`, `PUT /api/me/profiles/{id}` and `POST /api/me/profiles/validate`. Shared `decodeCabinetJSON*` for legacy routes untouched; unknown `expected_version` now 400 instead of silent 200. |
| F3 graph PUT partial mutation on 422 | MEDIUM | Graph is validated (`prepareGraphVersion`) and CAS-checked **before** `RenameProfile`/version append; invalid graph → 422 with name and version count unchanged (mirrors POST cleanup). |
| F5 `observe.arm` control output | LOW | Kept terminal (05 §4 row says output "нет"; both templates use it as a terminal arm). Registry left as-is with a clarifying comment; templates re-validated (0 diagnostics). |
| L legacy `profile_versions` fixture | LOW | `TestProfileVersionsLegacyRowBackfill`: pre-stage DB without the 4 columns + a text row → `Open` backfills `format="text"`, steps intact. |
| L `MaxRetryBackof` typo | LOW | Renamed to `MaxRetryBackoff`. |
| L `CanonicalBytes` RawMessage non-canonicality | LOW | Documented limitation on `CanonicalBytes` (numerically-equal literals hash differently); not changed to avoid hash churn. |
| L oversized body → 400 not 413 | LOW | Not changed (out of the requested fix set; envelope handler mapping shared with legacy routes). |

Added server tests: graph PUT stale CAS → 409 / correct CAS → 200 new version;
unknown envelope field → 400; invalid graph PUT → 422 with name and version
count unchanged; text PUT still appends a text version.

Verification (from `registry/`):

```
go vet ./...                                                        # clean
go test -race -count=2 ./internal/blueprint ./internal/control ./internal/server \
  -run 'Blueprint|Validate|ProfileGraph|Profile|Template'           # ok
go build -mod=readonly ./...                                        # ok
gofmt -l .                                                          # empty
git diff --check                                                    # clean
```
