# implementation-summary-multimaster.md — Multi-master per domain (DNS round-robin)

Branch: `dev` (uncommitted, on top of the M1 users/quotas + DC2 health working-tree
changes — none reverted, nothing committed). Registry module only.
Authoritative spec: `docs/specs/multi-master-dns.md`.

## Design in one line

`[selection] masters_per_domain` (1..8, default 1) gives every managed domain
`M` ordered master **slots**; selection works on slots, Cloudflare reconciles
the full desired A **set** per domain, SRMD capacity is scaled by `M`, and M=1
stays byte-for-byte the previous behavior apart from the list-shaped storage
format.

## Key decisions (5)

1. **Config** — `Selection.MastersPerDomain` (`masters_per_domain`), validated
   1..8, default 1 (`config.DefaultMastersPerDomain` / `MaxMastersPerDomain`,
   `ResolveMastersPerDomain`). It is independent of `[srmd] max_nodes_per_domain`.
2. **State** — `Assignments map[string]AssignmentSlots` (`[]string`) and
   `AssignmentsSince map[string]SinceSlots` (`[]time.Time`, index-aligned).
   `DNSOperation` fields are now set-shaped: `DesiredTargets/DesiredNodes/
   AppliedTargets []string`. Both legacy formats (single string / single
   timestamp / singular DNS keys) load as one-element lists; save is always the
   new format. `state_test.go` pins the new on-disk format + backward read.
3. **Selection core** (`selection_core.go`, pure, table-tested) — slot-based:
   `pickSlotCandidate` (least-loaded; different node, different IP),
   `reassignDead` replaces only a dead slot, `rotateByTTL` rotates **one slot
   per tick** using `AssignmentsSince[domain][i]`, `fillEmpty` fills missing
   slots then keeps the old single-domain rebalance. Empty queue never drops
   assignments. Events carry `Domain` + `slot i/N` detail when `M>1`.
4. **DNS** (`dns.go`) — `reconcileARecord(domain, ips)` reconciles the full
   desired set: creates missing A, deletes extra A, leaves matching A
   untouched, and still drops a conflicting CNAME. `applyDNSTargets(domain,
   []dnsMaster)` replaces the single-target upsert; `enqueueDNSDesiredLocked`
   takes slices; `recoverDNSDesiredLocked` rebuilds sets from multi-assignments.
5. **SRMD** (`srmd.go`) — `domain_capacity = max_nodes_per_domain ×
   masters_per_domain`; `srmdRequiredDomains(queue, max, M)` divides healthy
   queue size by that capacity (ceil, min 1), so M>1 collapses domains. Spec
   invariant: `srmdRequiredDomains(queue, 5, 2) == ceil(queue/10)`.
   `DomainClients[d]` is aggregated over all assigned masters. Fold/unfold
   thresholds use the same capacity; anti-flap (`srmdStableTicks`,
   `srmdMaxDomains`) unchanged.

Also required and done: `terminateNodeLocked` removes the node from **all**
slots of its domains and re-enqueues the (shrunken) desired DNS set; the panel
`/panel/api/overview` row lists masters `{node_id, ip, since}` plus
`slots_filled: "2/2"` (public stats/dashboard/links stay one link per domain);
the config editor's "re-write DNS" pushes the full A set per domain.

## Files changed

Production (registry, carried in the working tree):

| File | Change |
|---|---|
| `registry/internal/config/config.go` | `[selection] masters_per_domain`, bounds 1..8, resolve/validate, default |
| `registry/internal/state/state.go` | `AssignmentSlots`/`SinceSlots` with legacy `UnmarshalJSON`; set-shaped `DNSOperation` + `Drifted`/`sameStringSet` |
| `registry/internal/server/selection_core.go` | slot-based pure core (`pickSlotCandidate`, slot `reassignDead`/`rotateByTTL`/`fillEmpty`, `slotLabel`) |
| `registry/internal/server/dns.go` | full-set `reconcileARecord`, `applyDNSTargets`, slice `enqueueDNSDesiredLocked`, multi-assignment recovery |
| `registry/internal/server/srmd.go` | `srmdDomainCapacity`, scaled `srmdRequiredDomains`, aggregated `DomainClients`, panel `masters_per_domain` |
| `registry/internal/server/panel.go` | per-domain `masters[]` + `slots_filled` in overview |
| `registry/internal/server/terminate.go` | drop node from all slots on terminate |
| `registry/internal/server/run_main_impl.go` | `selectionView.Masters` from config; DNS desired rebuilt per changed domain |
| `registry/internal/server/deps_state.go` | `AssignmentSlots`/`SinceSlots` aliases |
| `registry/internal/server/config_editor.go` | "re-write DNS" pushes the full master A set |
| `registry/internal/server/stats.go` | per-domain master/link views use the first slot |

Tests:

| File | Change |
|---|---|
| `registry/internal/state/state_test.go` | round-trip pins list format; legacy string → one-element list; `Drifted` set semantics |
| `registry/internal/server/selection_core_test.go` | migrated to slot API + mandatory tests 1, 2, 3, 6; `firstMaster`/`firstSince` helpers |
| `registry/internal/server/srmd_test.go` | 3-arg `srmdRequiredDomains`; mandatory `…ScalesWithMasters` invariant |
| `registry/internal/server/dns_ops_test.go` | `applyDNSTargets`; mandatory test 5 (full-set reconcile) |
| `registry/internal/server/terminate_test.go` | mandatory test 7 (terminate with 2 masters) |
| `registry/internal/server/{main,masterttl,metricslatch,panel_node,stats,events,dc2_report}_test.go` | migrated to slot/set state |

## Mandatory test list (specs/multi-master-dns.md)

| # | Required behavior | Test(s) | Status |
|---|---|---|---|
| 1 | fill places 2 distinct nodes with distinct IPs; duplicate IP rejected | `TestFillSlotsDistinctNodesDistinctIPs`, `TestFillSlotsRejectsDuplicateIP` | ✅ |
| 2 | one of two masters dies → only its slot replaced, other stays | `TestReassignDeadReplacesOnlyDeadSlot` | ✅ |
| 3 | TTL rotates one slot per tick; per-slot `AssignmentsSince` | `TestRotateByTTLOneSlotPerTick`, `TestRotateByTTLOldestHolderSurrenders` | ✅ |
| 4 | legacy state (string) reads & saves as list; DNSOperation string → list | `TestLoadLegacySingleMasterAssignments`, `TestSaveLoadRoundTrip` | ✅ |
| 5 | DNS reconcile: add missing, delete extra, keep matching | `TestDNSReconcileFullSet` | ✅ |
| 6 | `masters_per_domain=1` → previous behavior | `TestFillEmptySingleMasterParity` + all migrated M=1 tests | ✅ |
| 7 | terminate with 2 masters: slot freed, second stays, DNS set shrinks | `TestTerminateDropsNodeFromSlotsAndShrinksDNS` | ✅ |

Plus the SRMD scaling invariant:
`TestSRMDRequiredDomainsScalesWithMasters` (incl. `srmdRequiredDomains(37,5,2)==4`),
and `TestSelectionMastersPerDomainBounds` (`config_security_test.go`).

## Verification

```
cd registry
gofmt -l .                      → clean
go vet ./...                    → clean
go test -race ./...             → ok (control, server, state packages)
go build -mod=readonly ./...    → ok
~/go/bin/golangci-lint run ./... → 0 issues
```

## Deviations / notes

1. **`srmd_test.go` legacy call sites** now pass `M=1` explicitly to keep the
   pre-existing table expectations; the new scaling is pinned separately.
2. **Panel backward compatibility**: per-domain rows keep the scalar
   `node_id`/`ip` (first slot) so the existing UI stays valid; `masters[]` +
   `slots_filled` are additive.
3. **Public pages** (stats/dashboard/links) keep one entry per domain (first
   slot); only the panel API exposes the full master list, matching the spec.
4. **`upsertARecord`** (single) is gone — `reconcileARecord` is the set-capable
   replacement and the single-IP case is its special case, per spec.

Not committed — left as working-tree changes on `dev` as instructed.
