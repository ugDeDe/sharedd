# Manual-owner narrow source review

**Verdict: conditional GO for the scoped source fix; NO-GO for an unconditional production retry from the supplied evidence alone.** No critical/high defect identified in the reviewed reclaim path. Status: **partially_verified (source only)**. No tests, builds, application execution, production access, commits or live endpoint calls performed.

Reviewed `fix-manual-install-owner.md`, `control/manual_install.go`, `server/cabinet_manual.go`, and relevant registration, restart, worker, quota, host and provision-secret helpers.

## Safety findings

- **Not generic takeover:** session + CSRF and saved-host ownership are enforced (`cabinet_manual.go:41-46,248-271`). Transaction rechecks the complete host snapshot, exact persisted operation/user/manual key/deterministic node/profile, empty provider ID and compatible links (`manual_install.go:20-61`). A new operation cannot reclaim an existing owner. Legacy reclaim requires an **existing failed** operation, system legacy owner, not deleting, matching recorded release user when nonzero, original recoverable secret and zero reservations (`131-172`). Orphaned installing is accepted only for an already active same-user binding, not legacy reclaim. Generic `AuthorizeNode` still refuses different owners (`nodes.go:53-64`).
- **Matching host:** acceptance targets the unchanged saved host/SSH credentials, checks competing persisted host:port/node claims, and refuses any candidate for a released node or another candidate on the host IP (`manual_install.go:33-79`; `cabinet_manual.go:468-486`). This establishes current-host consistency, not an independent historical IP proof when old linkage is empty.
- **No duplicate quota charge identified:** owner, reservation, operation state and linkage commit together; quota counts distinct node identities including pending cloud creates. Existing same-user reservation is reused; another owner's reservation is refused; failure rolls back (`manual_install.go:81-125`). Other legacy nodes' reservations do not block this recovery; a reservation for this exact node does.
- **No scoped deadlock identified:** transaction queries use `tx`, never the single-connection store pool. Claim lock precedes state lock; registration's earlier state lock is released before taking the claim lock (`run_main_impl.go:283-301`). Worker mutex is released before running callbacks (`provision_worker.go:46-64`). Registration/secret issuance and manual acceptance/dispatch are serialized; restart follows the same claim lock. This is a single-registry-process guarantee, not cross-replica exclusion.
- **Secret preservation:** existing ciphertext is decrypted before writes and ciphertext plus plaintext hash are compared again inside the transaction. Unreadable/missing copies fail closed, rather than rotating a live token (`cabinet_manual.go:489-510`; `manual_install.go:148-171`). Personal-secret registration does not rotate it (`run_main_impl.go:331-341`); provision output masks `node_secret` (`provision.go:466-485`).

## Evidence and remaining gate

User 4 / host 1 with empty links, failed operation 1 / `manual-1` / `vps4cfeae-8e2ee`, legacy system owner and **no reservation for that node** is compatible with this path: empty links are explicitly allowed, operation 1 is reused, profile 2 remains selected. Assuming no active claimant, nothing in those facts alone forces refusal.

Still required before authorizing recovery: persisted operation/provider/profile consistency; owner `deleting=0`; `released_user_id` either 0 (old release) or 4; no competing persisted host/operation/nondeleted cloud resource; enabled user 4 with capacity; valid saved SSH data and selectable profile; successful secret decryption with the unchanged master key and matching stored hash. Ciphertext/hash lengths alone do not prove recoverability.

| Severity | Location | Caveat / action |
|---|---|---|
| Medium | `manual_install.go:54,155-157`; `control.go:182-191` | Timestamp provenance is **inclusive Unix seconds**: `host.created_at <= owner.registered_at <= op.created_at`. Equality must be accepted because original writes can occur in one second. It is a compatibility heuristic, not proof of historical ownership/IP: host IDs are reusable and a same-second delete/recreate can be indistinguishable. Preserve the original saved host; do not recreate it or relax refusal checks. Confirm the actual timestamps and original-row history before recovery. |

No supplied timestamps establish that interval yet. If owner creation is outside it, this implementation returns 403 `owner_mismatch` even with empty linkage and no active claimant; investigate provenance rather than bypassing it. Runtime/concurrency verification remains intentionally unperformed. Re-review any changes to reclaim constraints or locking.
