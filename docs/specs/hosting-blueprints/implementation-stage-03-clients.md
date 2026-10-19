# Implementation — Stage 03: Provider clients (bounded)

**Status: `partially_verified`** — see §5. No commits, no live paid calls.
Date: 2026-10-17. Prior stages 01 (SSH liveness) and 02 (GP recovery) are
DONE and untouched.

## 1. Exact files (created/changed this stage)

| File | Change |
|---|---|
| `registry/internal/nodehost/methods.go` | NEW — typed API methods on the existing `Client` (transport from the previous partial run, unchanged except one edge fix) |
| `registry/internal/nodehost/client.go` | 1-line edge fix: empty response body no longer fails `json.Valid` (an empty-body redirect/302 must surface as status-mapped `provider_error`, not `invalid_json`) |
| `registry/internal/nodehost/client_test.go` | NEW — httptest matrix for transport + all methods |
| `registry/internal/nodehost/decimal_test.go` | NEW — Decimal exactness tests |
| `registry/internal/beget/balance.go` | NEW — hosting-API `BalanceClient`, separate from the cloud JWT client |
| `registry/internal/beget/balance_test.go` | NEW |
| `docs/specs/hosting-blueprints/02-provider-api.md` | read-only reference |
| `docs/specs/hosting-blueprints/03-cabinet-api.md` | read-only reference |

Untouched per scope: `node/`, sshx, provision, server recovery files,
`registry/internal/beget/client.go` (existing cloud client).

## 2. Public API surface

### nodehost (package `registry/internal/nodehost`)

Transport (pre-existing, from previous run): `New(apiKey)`, `Error` (safe
codes only), `do()` with idempotency no-replay (`GetBody=nil`), redirect deny
(`ErrUseLastResponse`), 1 MiB body cap, `json.Valid`, `maxBody`, 15s timeout.

New methods — all validate inputs **before I/O** and return
`*Error{Code:"invalid_input"}` on bad input:

- `Account(ctx) (*Account, error)` — GET /v1/account
- `AccountBalance(ctx) (*Balance, error)` — maps `{amount: account.balance (exact Decimal string), currency:"USD", scope:"nodehost_api"}`; no floats for money
- `Locations(ctx) ([]Location, error)` — GET /v1/catalog/locations
- `OSTemplates(ctx) ([]OSTemplate, error)` — GET /v1/catalog/os-templates
- `CPUModels(ctx, location string) ([]string, error)` — optional `location` query
- `DiskTypes(ctx) ([]DiskType, error)` — GET /v1/catalog/disk-types; live entries are objects `{id,name}` (2026-10-17), not strings
- `NetworkOptions(ctx, location) (*NetworkOptions, error)` — `location` required
- `Limits(ctx) (*Limits, error)` — GET /v1/catalog/limits
- `Estimate(ctx, EstimateRequest) (*Estimate, error)` — POST /v1/pricing/estimate, no side effects, no idempotency key; live success is **201** (200 also accepted); request carries only `cores,ram,disk,networkSpeed,period,location` — `name/cpu/diskType/os` are create-only (live 400 otherwise)
- `Create(ctx, CreateRequest, idemKey string) (*CreateResult, error)` — POST /v1/servers, expects **202**; `Idempotency-Key` header set only here; empty `operationId` in the 202 body → `invalid_response`; empty key / invalid fields → `invalid_input` before I/O
- `Operation(ctx, id) (*Operation, error)` — GET /v1/operations/{id}; a *failed* operation at 200 is returned as a value (`Status:"failed"`, `Error.Code` — sanitized: control chars stripped, ≤64 runes, provider message never carried), not collapsed into `provider_error`
- `ServersPage(ctx, cursor string, limit int) (*ServerPage, error)` — `limit` must be 1..100; caller loops on `NextCursor` and must guard against repeated cursors
- `Server(ctx, id) (*Server, error)` — full card; `password` captured only into the struct field (`json:"-"` on the public wire side via custom `UnmarshalJSON`)
- `Delete(ctx, id) (*DeleteResult, error)` — DELETE /v1/servers/{id}; result is not billing-release proof (confirm by read, caller's job)
- `IPSubnets(ctx, id) ([]Subnet, error)` — GET /v1/servers/{id}/ip-subnets
- `ChangeIP(ctx, id string, subnetID int, useSubnet bool) (*ChangeIPResult, error)` — POST /v1/servers/{id}/change-ip, expects **200**; body `{}` or `{"subnetId":N}`; no idempotency key (change-ip has none documented — never blindly retry)

Validation bounds: `name` ≤ 100 chars; enum-ish strings (location, period,
speed, diskType, os, cpu, promo) ≤ 64 chars, non-empty where required, period
restricted to `1d|3d|1w|1m|3m|6m|1y`; cores/ram/disk > 0; path IDs via
`validID` (≤256 chars, no `.`/`..`) + `url.PathEscape`; idempotency key
≤ 128 chars.

Error codes (safe, provider text never surfaces): `invalid_input`,
`invalid_request`, `credentials_invalid`, `invalid_json`, `invalid_response`,
`response_too_large`, `transport_error`, `request_canceled`,
`validation_failed` (400), `unauthorized` (401), `insufficient_funds` (402),
`forbidden` (403), `not_found` (404), `conflict` (409), `rate_limited` (429),
`provider_error` (everything else, incl. 5xx, 302, HTTP-200 business
`{"error":{...}}` objects). A mutation error never proves absence of a remote
side effect.

### beget (package `registry/internal/beget`)

- `BalanceClient{Base, HC, Login, APIPassword}`; constructor `NewBalanceClient(login, apiPassword string)` with 15s default `HC` timeout (context deadline still applies).
- `Balance(ctx) (*Balance, error)` — POST `{base}/api/user/getAccountInfo`,
  `Content-Type: application/x-www-form-urlencoded`, body
  `login=…&passwd=…&output_format=json` (form body, **never** query string),
  1 MiB response cap, `Balance{Amount, Currency, Scope}` with
  `Scope:"beget_hosting_unverified"`, `Currency:""` for wire `null`.
- Two-level envelope: success only when `status=="success"` **and**
  `answer.status=="success"`; then `answer.result.user_balance` parsed as an
  exact decimal (JSON number literal or quoted string; exponent forms
  expanded exactly; **no float64 conversion** — precision beyond float64 is
  preserved). Empty / non-numeric → `invalid_balance`, never `0`.
- `BalanceError{Code, Message, HTTPStatus}` — separate type from the cloud
  `APIError`; codes: `balance_credentials_missing` (empty credentials,
  fail-closed — never falls back to the cloud password),
  `transport_error`, `response_too_large`, `invalid_response`,
  `invalid_balance`, `invalid_request`, outer `status:error` →
  `error_code`/`error_text`, inner `answer.status:error` → first
   `answer.errors[]` entry. Provider text is sanitized (control chars
   stripped, ≤200 chars); credentials never appear in error text — provider
   `error_text` is masked against `Login`/`APIPassword` substrings (≥3 chars)
   with `***` before leaving the client.

## 3a. Review-fix round (2026-10-17, review-stage-03-clients.md F-1..F-4)

| Finding | Fix | Files |
|---|---|---|
| F-1 HIGH — beget balance followed redirects, forwarding `login`+`passwd` form body to the `Location` target | `Balance()` copies `HC` per call and sets `CheckRedirect → http.ErrUseLastResponse` before `Do` (mirrors nodehost client.go); a 30x surfaces as `invalid_response` with the last status | `registry/internal/beget/balance.go` |
| F-2 MEDIUM — non-JSON error bodies lost status mapping (`invalid_json`, status 0) | status check moved above `json.Valid`; only expected-status bodies reach JSON validation; non-JSON 400/5xx now map via `safeProviderError` preserving status | `registry/internal/nodehost/client.go` |
| F-3 MEDIUM — `Operation()` could never return a failed operation (generic envelope check swallowed it) | `do()` gained an `envelope bool` parameter (`doDecode`); `Operation` uses the skip-envelope path, decodes the 200 body, and maps `error.code` into a new `*OperationError{Code}` (boundCode: control chars stripped, ≤64 runes, provider message never carried). `Operation.Error` is no longer `*Error` | `registry/internal/nodehost/client.go`, `types.go`, `methods.go` |
| F-4 MEDIUM — provider `error_text` not scrubbed against credentials | `BalanceError.Message` is masked via `scrubMessage` (`Login`/`APIPassword` substrings ≥3 chars → `***`) on both outer and inner error paths | `registry/internal/beget/balance.go` |

New acceptance tests: `TestBalanceRedirectNotFollowed` (307 → second server never requested, `invalid_response`/HTTPStatus 307), `TestBalanceErrorMasksCredentials` (outer+inner `error_text` echo → no credential substring), `TestNonJSONErrorBodyKeepsStatus` (502 HTML → `provider_error`/502, 400 text → `validation_failed`), `TestOperationFailedCarriesSanitizedCode` (200 failed op → `Status:"failed"`, `Error.Code:"insufficient_funds"`, no provider message; sanitize+cap cases).

## 3. Test matrix and results

All commands from `registry/`. Result: **all pass**.

| # | Test (nodehost) | Covers | Result |
|---|---|---|---|
| 1 | `TestAccount` | path/method, Bearer header, no idempotency key outside create, parse | PASS (×2 runs) |
| 2 | `TestAccountBalance` | balance mapping USD / nodehost_api / exact amount | PASS |
| 3 | `TestCatalog` | locations, os-templates, cpu-models (with/without location query), disk-types, network-options, limits | PASS |
| 4 | `TestEstimate` | happy path | PASS |
| 5 | `TestInputValidationNoIO` | 15 invalid-input cases; server asserted **not reached** (atomic flag) | PASS |
| 6 | `TestCreate202` | 202, `Idempotency-Key: idem-1` present on create | PASS |
| 7 | `TestCreateEmptyOperationIDRejected` | 202 without operationId → `invalid_response` | PASS |
| 8 | `TestOperation` | happy path incl. `error:null` | PASS |
| 9 | `TestServersPagination` | 2-page cursor loop with repeated-cursor detection guard in the test loop; `limit` query asserted | PASS |
| 10 | `TestServerDetailPasswordCapturedOnlyInStruct` | struct holds password; re-marshal contains no `password` key / no secret text | PASS |
| 11 | `TestDelete` | method DELETE, refund parse | PASS |
| 12 | `TestIPSubnets` | happy path | PASS |
| 13 | `TestChangeIPBothVariants` | body `{}` vs `{"subnetId":18}`; no idempotency key | PASS |
| 14 | `TestErrorMapping` | 400/401/402/403/404/409/429/500/302 → safe codes, HTTPStatus preserved, provider text never leaked | PASS |
| 15 | `TestBusinessErrorOnHTTP200` | `{"error":{...}}` at 200 → `provider_error` | PASS |
| 16 | `TestInvalidJSON` | → `invalid_json` | PASS |
| 17 | `TestOversizedBody` | >1 MiB → `response_too_large` | PASS |
| 18 | `TestRedirectNotFollowed` | 302 to a second server: second server **not requested** (no credential forward), last response kept (`HTTPStatus==302`, `provider_error`) | PASS |
| 19 | `TestContextCanceled` | → `request_canceled` | PASS |
| 20 | `TestTransportFailure` | connection refused → `transport_error` | PASS |

| # | Test (nodehost decimal) | Covers | Result |
|---|---|---|---|
| 21 | `TestDecimalUnmarshal/precision` | 50-fraction-digit literal survives exactly | PASS |
| 22 | `…/exponent forms` | `1.5e3→1500`, `1e2→100`, `-2.5e-2→-0.025`, `1E1→10` | PASS |
| 23 | `…/string form` | `"12.34"`, `-12.5` | PASS |
| 24 | `…/invalid forms rejected` | `abc`, `""`, `1.2.3`, `0x10`, `+1`, `.5`, `5.`, `01`, `1e`, `1e999`, 200-digit | PASS |
| 25 | `…/zero value is not monetary zero` | zero `Decimal` marshals `"amount":""`, never `"0"` | PASS |
| 26 | `…/round trip` | `"9.90"` survives unmarshal→marshal unchanged | PASS |

| # | Test (beget balance) | Covers | Result |
|---|---|---|---|
| 27 | `TestBalanceFormBodyNotQuery` | urlencoded **body** (PostForm), empty RawQuery, correct Content-Type, output_format=json; parse `1339.57` | PASS |
| 28 | `TestBalanceSuccess` | two-level success envelope | PASS |
| 29 | `TestBalanceStringDecimalAndPrecision` | `"142.50"` string form; 40-digit number preserved beyond float64 | PASS |
| 30 | `TestBalanceOuterError` | `status:error` → `error_code`/`error_text`; credentials absent from error text | PASS |
| 31 | `TestBalanceInnerErrors` | first `answer.errors[]` entry used | PASS |
| 32 | `TestBalanceMissingUserBalance` | missing / non-numeric / empty `user_balance` → `invalid_balance` (not 0) | PASS |
| 33 | `TestBalanceTimeout` | short HC timeout vs slow handler → `transport_error` | PASS |
| 34 | `TestBalanceOversized` | >1 MiB → `response_too_large` | PASS |
| 35 | `TestBalanceCredentialsMissing` | empty login or password → `balance_credentials_missing`, no I/O | PASS |
| 36 | `TestBalanceInvalidEnvelope` | non-JSON → `invalid_response` | PASS |

Post-fix additions (review-fix round, §3a):

| # | Test | Covers | Result |
|---|---|---|---|
| 37 | `TestBalanceRedirectNotFollowed` | 307 with `Location` to a second server: second server **not requested**, result `invalid_response` with `HTTPStatus==307` (F-1) | PASS (×2 runs) |
| 38 | `TestBalanceErrorMasksCredentials` | outer + inner `error_text` echoing `my-login`/`my-pass` → `Message` contains neither, contains `***` (F-4) | PASS |
| 39 | `TestNonJSONErrorBodyKeepsStatus` | 502 + HTML → `provider_error` with `HTTPStatus==502`; 400 + text → `validation_failed` (F-2) | PASS |
| 40 | `TestOperationFailedCarriesSanitizedCode` | 200 `{"status":"failed","error":{"code":"insufficient_funds","message":…}}` → `Status=="failed"`, `Error.Code=="insufficient_funds"`, provider message not leaked; control chars stripped, code capped at 64 (F-3) | PASS |

## 4. Explicit NOT-DONE list (out of this stage's scope)

- **Storage** — hosting connections, resources, idempotency records, balance snapshots, CAS-by-revision (`04-storage-and-execution.md`).
- **Cabinet routes** — all proposed `/api/me/hosting-connections*`, `/api/me/cloud-resources*`, management policy, profile-runs/blueprint endpoints (`03-cabinet-api.md` §3–§7). No `mountCabinet` changes.
- **Adapters** — outcome normalization layer (`confirmed|rejected|unknown`), reconcile workers, operation polling loop (`next_poll_at`), Beget cloud VPS/IP lifecycle methods beyond the existing client, credential-store worker consuming `Server.Password`.
- **UI** — no `ui/` changes, no regenerated webassets.
- **Graphs** — blueprint schema, validator, run engine.
- **Not verified against live providers** — no live Nodehost/Beget calls were made (per scope); wire shapes follow the 16.10.2026 source review in `02-provider-api.md`.

## 5. Verification status: `verified` (after review-fix round §3a)

| Check | Command | Result |
|---|---|---|
| vet | `go vet ./...` (registry) | PASS, clean |
| tests | `go test -race -count=2 ./internal/nodehost ./internal/beget` | PASS |
| tests, full module | `go test -race ./...` (registry) | PASS — 512 tests, 14 packages |
| build | `go build -mod=readonly ./...` | PASS |
| gofmt | `gofmt -l .` | empty (clean) |
| git | `git diff --check` | clean |
| golangci-lint | — | **UNAVAILABLE** — binary not installed in this environment; CI will run it |

`node/` module not re-run (untouched this stage, per scope). No commits made.

## 6. Live verification note (2026-10-17)

Read-only Nodehost calls were verified **live** with a user-provided test key
(key, account email and username never recorded anywhere in repo/tests/docs).
Confirmed and aligned into the client (`methods.go`, `types.go`,
`client_test.go`) and `02-provider-api.md`:

- `disk-types` → objects `[{id,name}]` (was strings);
- `limits` → `{types:{...:bool},limits:{cpu,ram,disk:{min,max}}}`;
- `os-templates` → entries `{id,name}`, `title` absent (optional field kept);
- `estimate` → success **201** (200 also accepted); request must not carry
  `name/cpu/diskType/os` (live 400 "property X should not exist").

Everything else matched (account shape, locations, cpu-models, servers
pagination, 401 error mapping). **Mutations (create/delete/change-ip) remain
unverified against live** — read-only calls only.
