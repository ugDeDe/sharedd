# Review — Stage 03: Provider clients (bounded, read-only)

Date: 2026-10-17. Reviewer: go-reviewer. Scope: only the files listed in
`implementation-stage-03-clients.md` §1. No app edits were made by this
review; a redirect-behavior probe ran outside the repo (`/tmp/opencode`).

## 1. Verification performed

| Check | Command | Result |
|---|---|---|
| vet | `go vet ./internal/nodehost ./internal/beget` (registry) | PASS, clean |
| focused tests | `go test -race -count=2 ./internal/nodehost ./internal/beget` | PASS (1.1s / 2.1s) |
| gofmt | `gofmt -l internal/nodehost internal/beget` | empty |
| staticcheck / golangci-lint | — | UNAVAILABLE in this environment (matches stage doc §5); CI will run |
| test-matrix audit | read all 36 claimed tests vs prose | all exist and match claims |
| redirect probe | standalone 307 test in /tmp (not the repo) | confirmed POST body incl. `passwd` is re-sent to redirect target under default `CheckRedirect` |

## 2. Contract verification vs 02-provider-api.md

| Contract item | Status | Evidence |
|---|---|---|
| Money exactness, no float anywhere (incl. marshal) | PASS | `nodehost/decimal.go` — `Decimal` is a string; unmarshal via raw literal + regex + `json.Number`, never `float64`; marshals as JSON string (round-trip test pins `"9.90"`). Beget `normalizeAmount` operates on `json.RawMessage`; exponent forms expanded exactly. No `float64` conversions exist in either package. |
| Credentials never in errors/logs/query | PASS with gap (F-4) | nodehost `Error.Error()` = code only; provider text leak tested (`TestErrorMapping`). Beget query: `form.Encode()` in POST body; `TestBalanceFormBodyNotQuery` asserts empty `RawQuery`. Gap F-4: provider `error_text` is not scrubbed against credential substrings. |
| No blind retries of mutations | PASS | Single attempt everywhere; no retry loops. `req.GetBody = nil` (client.go:110) kills transparent replay on reused-connection failure. ChangeIP carries no key and has no retry (methods.go:220-241). |
| Redirect deny | nodehost PASS / beget FAIL (F-1) | nodehost sets `ErrUseLastResponse` per call; `TestRedirectNotFollowed` proves the second host is never requested. beget `BalanceClient` uses the default policy — see F-1. |
| Body caps | PASS | 1 MiB `LimitReader(+1)` in both; oversized → `response_too_large`, both tested. |
| JSON validation order | PARTIAL (F-2) | Order is json.Valid → status → envelope → unmarshal. Non-JSON bodies at 400/5xx map to `invalid_json` (status 0) instead of status-mapped codes — F-2. The empty-body edge was fixed; the non-empty non-JSON error-body edge was not. |
| HTTP 200 business errors | PASS generic / PARTIAL for Operation (F-3) | `{"error":{...}}` at 200 → `provider_error` (tested). For `GET /v1/operations/{id}` the documented `error` field of a *failed* operation is also swallowed by the generic envelope check — F-3. |
| Idempotency key only on create | PASS | Header set only when key ≠ "" ; `TestAccount` + `TestChangeIPBothVariants` assert absence; `TestCreate202` asserts presence. |
| Change-ip single attempt, no idempotency | PASS | See above; body `{}` / `{"subnetId":N}` both pinned. |
| Password never leaks via DTO marshal/log | PASS | `Server.Password json:"-"` + custom `UnmarshalJSON`; `TestServerDetailPasswordCapturedOnlyInStruct` re-marshals and greps for the secret. |
| Beget form body not query | PASS | `TestBalanceFormBodyNotQuery` uses `PostFormValue` (body only). |
| Two-level envelope | PASS | Outer `status=="success"` AND `answer.status=="success"` required; outer `status:error` → `error_code/error_text`; inner → first `answer.errors[]`; other shapes → `invalid_response`. All tested. |
| Exact decimal / currency null / scope unverified | PASS | `Currency:""` for wire null; `Scope:"beget_hosting_unverified"`; empty/non-numeric `user_balance` → `invalid_balance`, never `0`. |
| Error code table matches spec (400/402/403/409/429/5xx) | PASS modulo F-2 | 400→`validation_failed`, 402→`insufficient_funds`, 403→`forbidden`, 409→`conflict`, 429→`rate_limited`, 5xx/302→`provider_error`; extra 401→`unauthorized` is sensible. `Retry-After` is not parsed (note N-1). |
| Input bounds / path traversal / injection | PASS | `validID` (≤256, non-empty, no control chars, rejects `.`/`..`) + `url.PathEscape` for every path ID; query values via `url.Values`/`QueryEscape`; `text()` bans control chars (no CRLF games). `TestInputValidationNoIO` proves 15 bad inputs never reach the wire (atomic flag). |
| Concurrency | PASS | nodehost `Client` is immutable after `New` (key/base/hc); `do()` copies `hc` per call and sets `CheckRedirect` on the copy only — no shared mutable state, safe for concurrent use. beget `BalanceClient` has exported mutable fields (test injection); safe provided callers don't mutate fields during in-flight calls. `-race -count=2` clean. |
| Test matrix genuine | PASS | All 36 claimed tests exist, match the prose, and pass under `-race -count=2`. Spot-checks confirm the strongest claims (no-IO validation, redirect non-follow, secret round-trip, form-not-query, precision >float64). |

## 3. NEW findings

### F-1 · HIGH — beget balance client follows redirects, forwarding credentials
`registry/internal/beget/balance.go:164` — `c.HC.Do(req)` uses the default
redirect policy. The request body carries `login` + `passwd`. A 307/308 from
the base host re-sends the full form body to the `Location` target (proven by
probe: `body="login=victim&...&passwd=hunter2"` landed on the second host).
`02-provider-api.md` §1 makes redirect-with-credentials to another host a
general adapter rule; the nodehost client implements it, this one forgot.

Fix (balance.go, inside `Balance`, before `Do`):
```go
hc := *c.HC
hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
resp, err := hc.Do(req)
```
Acceptance test: handler answers 307 with `Location` to a second server;
assert the second server is never hit and the result is `invalid_response`
with `HTTPStatus 307` (30x bodies parse as the last response, credentials
never leave the first host).

### F-2 · MEDIUM — non-JSON error bodies lose their HTTP mapping
`registry/internal/nodehost/client.go:125` — `json.Valid` runs before the
status check, so `400`/`5xx` responses with an HTML/plain-text body return
`invalid_json` with `HTTPStatus 0`. The caller cannot apply the spec table
(400 = fix input, no purchase retry; 5xx/timeout = unknown + read
reconciliation) and loses the status. The stage edge-fix only covered *empty*
bodies.

Fix: move `if resp.StatusCode != expected { return safeProviderError(...) }`
above the `json.Valid` check and keep `json.Valid`/envelope checks only for
expected-status bodies.
Acceptance test: 502 + `<html>oops</html>` → `provider_error` with
`HTTPStatus 502`; 400 + `oops` → `validation_failed`.

### F-3 · MEDIUM — `Operation()` can never return a failed operation
`registry/internal/nodehost/client.go:135-141` vs `types.go:84-91`. The spec
documents `error` as part of the 200 body of `GET /v1/operations/{id}`
("Failed может прийти после исходного 202, включая недостаток средств"). The
generic envelope check converts any failed-operation body at 200 into
`provider_error`, dropping `status`/`resourceId`; insufficient funds becomes
indistinguishable from any other failure at the method level. Additionally
`Operation.Error *Error` would decode provider `code` text into the type whose
doc says "never provider text" — dead today (envelope check fires first), but
inconsistent.

Fix (either): give `Operation` its own do-variant that skips the generic
envelope check, decodes the body, and maps provider `error.code` into a
bounded, sanitized field (or drops it); or keep the generic mapping and
document on the method that failed-operation outcomes arrive as
`provider_error`(HTTP 200) and the failed-state polling layer belongs to stage
04 — then remove or repurpose the dead `Error` field.
Acceptance test: 200 `{"id":"op1","status":"failed","resourceId":"","error":{"code":"insufficient_funds"}}`
yields a distinguishable outcome with no provider text beyond the sanitized
code.

### F-4 · MEDIUM — provider `error_text` not scrubbed against credentials
`registry/internal/beget/balance.go:89-100,186,196` — `sanitizeBound` strips
control chars and caps length, but if Beget echoes the login inside
`error_text` (common for auth failures), the credential substring rides in the
exported `BalanceError.Message` on future log/DTO paths. The stage doc claims
"credentials never appear in error text"; nothing enforces it (tests only
check `Error()`, which returns the code alone).

Fix: after building each `BalanceError`, mask known secrets:
```go
msg := sanitizeBound(env.ErrorText)
msg = strings.ReplaceAll(msg, c.Login, "***")
msg = strings.ReplaceAll(msg, c.APIPassword, "***")
```
Acceptance test: outer error with `error_text:"user my-login not found"` →
`Message` contains no `my-login`.

## 4. Notes (no action required for this stage)

- N-1: `Error` does not carry `Retry-After`; spec §1 wants 429 backoff to
  honor it. If the stage-04 reconcile layer needs the value, add a bounded
  int-seconds field then — parsing a header now without a consumer is dead
  weight.
- N-2: `Estimate`/`Create` validate shapes (>0, length/enum bounds) but not
  against `/v1/catalog/limits`; per spec that cross-check belongs to the
  adapter.
- N-3: beget success envelope is accepted at any HTTP status; a non-200 with a
  valid success body would pass. One-line `resp.StatusCode != 200 → provider_error`
  if defense-in-depth is wanted.
- N-4: zero `Decimal` marshals `""` rather than `null`; §5 wants `null` for
  "no snapshot yet" in the *cabinet* DTO — that mapping is stage 04's job.
- Acknowledged limitations (storage, adapters/reconcile/polling, cabinet
  routes, UI, no live provider calls, golangci-lint unavailable) match the
  stage doc's NOT-DONE list and are not counted as findings.

## 5. Summary

| Severity | Count | Status |
|----------|-------|--------|
| CRITICAL | 0 | pass |
| HIGH     | 1 | block  |
| MEDIUM   | 3 | block  |

Verdict: **BLOCK** — F-1 is a spec-mandated credential-protection control
missing from a credential-carrying request; F-2/F-3 break the error-code
contract this stage explicitly claims complete. All four fixes are small and
testable; after they land (plus `go vet` / `-race` re-run), the stage is
otherwise sound: money handling, retry discipline, envelope parsing,
validation bounds, concurrency, and the test matrix check out.

## 6. Re-review verdict — F-1..F-4 (2026-10-17)

Focused re-review of the §3a fix round (implementation doc §3a, tests 37–40).
Scope: `beget/balance.go`, `beget/balance_test.go`, `nodehost/client.go`,
`nodehost/client_test.go`, `nodehost/types.go`, `nodehost/methods.go`.

| Finding | Status | Evidence |
|---|---|---|
| F-1 redirect deny on credential path | FIXED | `balance.go:180-182` — `HC` copied per call, `CheckRedirect → http.ErrUseLastResponse` before `Do`. `TestBalanceRedirectNotFollowed` proves the 307 `Location` target is never requested (atomic `secondHit` flag) and the last response surfaces as `invalid_response` with `HTTPStatus==307`; credentials stay on the first host. |
| F-2 status before `json.Valid` | FIXED | `client.go:151-155` — `resp.StatusCode != expected → safeProviderError` now runs before the `json.Valid` check; only expected-status bodies reach JSON validation. `TestNonJSONErrorBodyKeepsStatus`: 502+HTML → `provider_error`/502, 400+text → `validation_failed`, status preserved on `*Error`. |
| F-3 failed operation distinguishable, sanitized code | FIXED | `doDecode` (`client.go:109`) takes an `envelope` flag; `Operation` (`methods.go:163`) skips the generic envelope check, decodes the 200 body, and maps `error.code` through `boundCode` (control chars stripped, ≤64 runes) into `OperationError{Code}` (`types.go:89-99`). Provider `message` is not even a struct field — it cannot ride along. `TestOperationFailedCarriesSanitizedCode` covers outcome distinction (`Status:"failed"`, `Error.Code:"insufficient_funds"`), leak check, sanitize, and cap. |
| F-4 credential masking in error text | FIXED | `scrubMessage` (`balance.go:52-60`) masks `Login`/`APIPassword` substrings (≥3 chars) with `***` on both outer (`:204`) and inner (`:214`) error paths. `TestBalanceErrorMasksCredentials` asserts neither credential appears and `***` is present, for both paths. |

No new regressions: the F-2 reorder leaves all prior mappings intact
(`TestErrorMapping` 302→`provider_error`, `TestInvalidJSON`→`invalid_json`,
`TestBusinessErrorOnHTTP200`→`provider_error` all pass unchanged); F-3's
skip-envelope path is used only by `Operation`; F-1 mirrors the proven
nodehost pattern. One residual note (unchanged scope): a 200 body that is a
top-level envelope business error on `Operation` now decodes into a mostly
empty `Operation` instead of `provider_error` — inherent to the fix option
chosen and documented on the method; the failed-op polling layer (stage 04)
owns the interpretation.

## 7. Re-review verification

| Check | Command | Result |
|---|---|---|
| vet | `go vet ./internal/nodehost ./internal/beget` (registry) | PASS, clean |
| focused tests | `go test -race -count=3 ./internal/nodehost ./internal/beget` | PASS (nodehost 1.1s, beget 2.6s; ×3 guards flakiness) |
| gofmt | `gofmt -l .` (registry) | empty |
| staticcheck / golangci-lint | — | still unavailable in this environment; CI runs them |

## 8. Re-review summary

| Severity | Count | Status |
|----------|-------|--------|
| CRITICAL | 0 | pass |
| HIGH     | 0 | pass |
| MEDIUM   | 0 | pass |

Re-review verdict: **PASS** — all four findings resolved as specified, all
40 tests pass under `-race -count=3`, no new findings. Stage 03 clients are
approved.
