# Editable standard vars — backend implementation / UI handoff (dev)

Inputs: `docs/specs/editable-standard-vars.md`,
`docs/specs/fix-profile-control-errors.md`, `DESIGN.md`.
Backend only; UI implementation/QA is delegated next. Dirty tree preserved;
no deployment, service start/restart, production actions, profile/DB writes,
tests, commits or pushes. Existing live profiles and `DefaultProfileVarsSchema`
were not rewritten. No new dependencies, configuration, migrations or ad-tag
defaults were introduced.

## Exact merge, empty and reset semantics

All three install entrypoints share `control.MergeProfileVars` then
`Registry.provisionVars`:

- `POST /api/me/vps`
- `POST /api/me/servers/{id}/install` (also manual retry)
- `POST /api/me/operations/{id}/retry-install`

Keys match exactly (case-sensitive). For `proxy_port`, `proxy_sni`,
`proxy_secret`: present request key wins; otherwise use a **nonempty** declared
profile default; otherwise inject the current registry default. Omitted `vars`
and `{}` both mean no install overrides. Schema declaration missing `default`
and `default:""` both mean inheritance, not an explicit empty override.
Whitespace defaults are nonempty (not trimmed).
Existing duplicate-declaration behavior is preserved: for an absent request
key, the first nonempty declaration default wins; an earlier blank does not
block a later nonempty default. No stored schema normalization was introduced.

| Value/key | Result |
|---|---|
| Omitted request key | Nonempty profile default, else registry default |
| Reset override | UI deletes request key; same as omitted, not a stored reset operation |
| Request `proxy_secret:""` | Explicit empty wins, suppresses profile/registry secret |
| Request `proxy_port:""` / `proxy_sni:""` | `400 validation_failed`, never fallback |
| Request port/SNI `null` | Invalid, not reset/inheritance |
| Request `ad_tag:""` or ordinary custom empty | Preserved; suppresses nonempty declared default |
| Schema `default:""` | No merged key added; registry fallback only for the three proxy keys |

Ordinary variable types retain the existing `fmt.Sprint` rendering behavior;
`null` is not a reset token (for example a `proxy_secret:null` value renders
`<nil>`, not empty/inherited). UI should submit strings for editable values.

The three registry defaults are `[shared_proxy].port`, `.tls_domain`, and
the client secret of the lexicographically first username in `.users` (sole
entry uses its secret; no entries gives empty). The selection rule is unchanged.
No aliases: legacy ordinary `port` is **not** `proxy_port`; `ad_tag_*` is **not**
`ad_tag`. `ad_tag`/valid `ad_tag_*` and declared/custom ordinary variables remain
request > nonempty declared default, with no injected ad-tag value.

Validation rejects port unless decimal digits resolve to 1..65535 (string or
integer JSON number). Signs, whitespace, fractional values, boolean, null and
composite values fail. Numeric `443.0` decoded as an integral number is accepted;
string `"443.0"` is not. Leading zeros in a digit string are accepted and retained.
SNI must be a string accepted by `config.ValidHostname`: ASCII dot-separated
domain, max 253 bytes, labels 1..63, letters/digits/hyphens, labels cannot
start/end with hyphen. No blank, underscore, wildcard, trailing dot, scheme,
path, port, IP literal or whitespace; no server trimming. Reuses registry
shared-proxy hostname rules plus stdlib `net.ParseIP` to exclude IP literals
from SNI, not a new domain parser.

Nonempty port/SNI defaults are validated at profile save and install. An invalid
legacy default is rejected even if the request would override it; fix via an
explicit version edit, not an automatic rewrite. Existing variable name/size
limits and shell quoting remain in force. Errors do not echo input values.

## Protected identity and security review

Declarations and request keys `node_id`, `node_secret`, `registry_url` are
rejected even with no default or an empty value, before quota/paid/install
side effects. Legacy declarations remain stored/readable for a UI diagnostic;
they cannot be silently used during installation. Ordinary name validation
still applies. Generic `ValidateExtraVars` accepts server-injected identity;
editable-key enforcement lives at schema/merge boundaries and defensively in
`provisionVars`, not in the post-injection validator.

`node_id` remains server-created; node-secret issuance/recovery/rotation is
unchanged. `registry_url` is always server-configured `panel.public_url`, including
empty; empty configuration never preserves caller input. No identity values
are added to defaults metadata. No common `security.node_token` is injected.
Existing auth/CSRF/ownership/quota guards, encryption and output secret masking
remain unchanged. Only installation vars change; registry machine config,
node master eligibility, shared port enforcement and antiscan are untouched.

## Authorized metadata endpoint

`GET /api/me/install-defaults`, mounted in `mountCabinet`, uses `requireSession`
and existing fail-closed users/control/session/disabled checks. Read-only; no
CSRF required. Returns `200 application/json`, `Cache-Control: no-store`:

```json
{"proxy_port":443,"proxy_sni":"proxy.example.org","proxy_secret_configured":true}
```

Example only, not hardcoded values. Exactly those three properties; no wrapper.
Boolean reports whether the **selected** registry proxy secret is nonempty, not
whether any user entry has a secret. It is not a reveal capability. No plaintext
`proxy_secret`, registry `node_token`, `node_secret`, identity metadata or merged
vars. Cabinet cannot substitute panel Bearer for its session. Errors:
401 `unauthorized`, 403 `user_disabled`, 503 `users_disabled` or
`control_unavailable` (including failed session lookup).

Admin editor can use existing `GET /panel/api/config` with panel authorization.
Cabinet must never use it. Profile responses continue exposing stored
`vars_schema`, not inherited/effective defaults; `ProfileVar.Default` already
uses `omitempty` and remains unchanged. The new endpoint does not return a blank
secret field that might be serialized accidentally as an override.

## UI delegation requirements (not implemented here)

- Follow `editable-standard-vars.md`: four separate ordinary standard rows,
  prominent steps textarea, protected read-only reference, existing shared UI.
- Track explicit overrides separately from displayed inherited values. Untouched
  or reset fields are **omitted** from request vars, including inherited blanks.
  Intentional empty secret/ad-tag values remain present. Never copy metadata
  wholesale into request vars or into saved profile declarations.
- Do not persist synthetic declarations on open or metadata-only save; preserve
  existing default vars. Always mask `proxy_secret`, irrespective of schema flag.
  For registry secret inheritance display «Задаётся регистратором», never a fake
  plaintext value; boolean does not permit reveal. Do not render/reveal/copy a
  `node_secret` value or request it as metadata.
- Endpoint load/error/unavailable states are not known-empty defaults. Fetch
  once for the installation UI using cabinet session, preserving unsent edits.
  Actual defaults may change before submit; server uses config at installation.
- Always show by `proxy_port`: «Порт установки должен совпадать с
  [shared_proxy].port регистратора. Изменение здесь не меняет настройки
  регистратора или допуск ноды к мастерству.» This is not SSH port.
- Profile changes reset/warn on unsent profile-specific edits. Retry remerges
  the supplied request with the selected profile and current registry defaults;
  previous install overrides are not implicitly reused by an omitted key.
  Existing restart resume merges schema with no request vars (same pre-existing
  limitation: no new persisted override snapshot/secret storage added).

## Files and permitted verification

Changed backend files:
- `registry/internal/control/profiles.go`: editable-key protection, port/SNI
  validation at save/merge/execution; nonempty-default merge unchanged.
- `registry/internal/server/cabinet_beget.go`: presence-based proxy defaults,
  protected URL enforcement, defensive validation; secret recovery unchanged.
- `registry/internal/server/cabinet_api.go`: session-authorized whitelist GET.

Docs: `docs/api-contract.md`, this file. No UI/generated asset changes.
No tests added or run by explicit request. Allowed checks from `registry/`:

```sh
gofmt -w internal/control/profiles.go internal/server/cabinet_beget.go internal/server/cabinet_api.go
go vet ./internal/control ./internal/server
go build -mod=readonly ./internal/control ./internal/server
```

Verification status: **partially_verified**. Targeted gofmt, vet and readonly
package build above passed; source review confirmed all install entrypoints use
the merge boundary and GET uses the session guard/whitelist. `git diff --check`
also passed for tracked dirty files (changed backend files are already untracked
in this working tree). No tests added/run by request; no runtime/browser QA or
independent reviewer execution. UI warning/override-state changes remain delegated.
Return to leader for UI delegation, not production rollout.
