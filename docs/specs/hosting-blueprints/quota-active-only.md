# Quota counts only pool nodes

Updated 2026-10-19; replaces the earlier pending-install reservation semantics.

```sql
SELECT COUNT(*) FROM quota_reservations WHERE user_id = ?;
```

- `node_owners` is durable identity/ownership, not usage.
- `pending_node_secrets` is an installer credential awaiting claim, not usage.
- Creates, cloud/manual installs and legacy Beget orders do not check or
  consume pool quota. Existing monetary idempotency, cooldown, daily limits
  and unknown-outcome safeguards remain.
- `AuthorizeNode` and `ClaimPendingNodeSecret` take the SQLite write lock before
  reading evidence. A newly inserted reservation is checked against the current
  DB `max_nodes`; overage rolls the whole transaction back. Failed claims retain
  their pending credential. Existing-owner recovery must reacquire a slot too.
- Repeat admission of an already-reserved node is idempotent, including after
  an administrator reduces the limit. Disabled accounts still cannot register.
- Personal-secret recovery goes through admission, not an owner-only bypass.
- Reinstall acceptance preserves durable ownership and any working candidate's
  reservation. Changing node identity cannot evict that candidate for free;
  when it actually leaves the pool, its reservation is released normally.
- Startup `ReconcileQuota(candidateIDs)` replaces reservations atomically with
  the persisted candidate set (non-legacy owners only). It removes stale rows
  and seeds missing live rows; readers never see an intermediate empty set.
  `EnsureQuota` remains the idempotent single-node seed helper.

Prune, expiry, terminate and same-IP replacement release only the departed
candidate's reservation. Their existing ownership preservation is unchanged.

## Pending credential cleanup

Existing cleanup is retained: confirmed cloud deletion revokes that resource's
pending install credential; safe local deletion and manual-host deletion do so
transactionally. Startup and periodic `ReconcileOrphanPendingSecrets` remove
terminal failed/cancelled/deleted installs with no non-deleted resource or saved
host. Retryable installs on existing targets retain their pending credential.
Cleanup is credential hygiene, no longer quota accounting.

An absent worker/external ID does not prove an absent paid VPS. Unknown create
outcomes must be reconciled; the application blocks local deletion meanwhile.

Focused check: `TestUsedNodesCountsManualPending` now asserts pending=0,
claimed/live=1, quota-denied recovery with no leaks, concurrent one-slot claims,
startup stale-row cleanup/live preservation, and provider-handle migration and
durability. See `lifecycle-live-fixes.md` for the exact executed commands.
