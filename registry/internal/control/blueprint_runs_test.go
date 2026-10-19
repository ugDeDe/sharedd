package control

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func testGraphVersion(t *testing.T, s *Store, uid int64) ProfileVersion {
	t.Helper()
	p, err := s.CreateProfile(fmt.Sprintf("g-%s-%d-%d", t.Name(), uid, time.Now().UnixNano()), "", uid)
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	v, err := s.CreateProfileVersionGraph(p.ID, `{"schema_version":1}`, 1, "hash")
	if err != nil {
		t.Fatalf("graph version: %v", err)
	}
	return v
}

func TestProfileRunClaimAndCheckpointLease(t *testing.T) {
	s := openTest(t)
	u := mustUser(t, s, "runowner", 5)
	pv := testGraphVersion(t, s, u.ID)
	run, err := s.CreateRun(ProfileRun{
		UserID: u.ID, ProfileVersionID: pv.ID, ProfileID: pv.ProfileID,
		NodeID: "pool-abc12", Entry: "initial",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	// Stale revision cannot claim.
	if _, ok, _ := s.ClaimRun(run.ID, run.Revision+5); ok {
		t.Fatal("stale revision must not claim")
	}
	claimed, ok, err := s.ClaimRun(run.ID, run.Revision)
	if err != nil || !ok || claimed.State != RunRunning {
		t.Fatalf("claim: %+v ok=%v err=%v", claimed, ok, err)
	}
	// Second worker with the old revision must lose.
	if _, ok, _ := s.ClaimRun(run.ID, run.Revision); ok {
		t.Fatal("double claim with stale revision must fail")
	}

	cp, err := s.SaveCheckpoint(BlockCheckpoint{
		RunID: run.ID, NodeID: "create", State: CheckpointReady, LeaseGeneration: 1,
	})
	if err != nil {
		t.Fatalf("save cp: %v", err)
	}
	if _, err := s.SaveCheckpoint(BlockCheckpoint{
		RunID: run.ID, NodeID: "create", State: CheckpointDispatched, LeaseGeneration: 2,
	}); err != nil {
		t.Fatalf("newer lease: %v", err)
	}
	// Stale worker cannot overwrite a newer lease.
	if _, err := s.SaveCheckpoint(BlockCheckpoint{
		RunID: run.ID, NodeID: "create", State: CheckpointDispatched, LeaseGeneration: 1,
	}); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale lease: want ErrRevisionConflict, got %v", err)
	}
	if _, err := s.CompleteCheckpoint(run.ID, "create", 0, 2, CheckpointConfirmed, "{}", ""); err != nil {
		t.Fatalf("complete: %v", err)
	}
	// Confirmed checkpoints are immutable.
	if _, err := s.SaveCheckpoint(BlockCheckpoint{
		RunID: run.ID, NodeID: "create", State: CheckpointDispatched, LeaseGeneration: 3,
	}); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("confirmed overwrite: want ErrRevisionConflict, got %v", err)
	}
	// UNIQUE(run,node,invocation): exactly one row.
	cps, err := s.CheckpointsByRun(run.ID)
	if err != nil || len(cps) != 1 || cps[0].ID != cp.ID {
		t.Fatalf("checkpoint uniqueness: %+v err=%v", cps, err)
	}
}

func TestBanDeliveryDedupeAndProfileWatermark(t *testing.T) {
	s := openTest(t)
	u := mustUser(t, s, "banowner", 5)
	pv := testGraphVersion(t, s, u.ID)
	if _, err := s.SetNodeManagementProfile("pool-abc12", u.ID, 1, pv.ID, `{}`, false, 0, 1); err != nil {
		t.Fatalf("enable profile: %v", err)
	}
	if err := s.AddBanEpisode("pool-abc12", "203.0.113.9", "ep_1"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddBanEpisode("pool-abc12", "203.0.113.9", "ep_1"); err != nil {
		t.Fatal(err)
	}
	cands, err := s.UndeliveredBanEpisodes()
	if err != nil {
		t.Fatal(err)
	}
	// activated_at=1, episode created now > 1 → deliverable.
	if len(cands) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(cands))
	}
	c := cands[0]
	_, created, err := s.CreateRunForBan(c, ProfileRun{})
	if err != nil || !created {
		t.Fatalf("first delivery: created=%v err=%v", created, err)
	}
	_, created, err = s.CreateRunForBan(c, ProfileRun{})
	if err != nil || created {
		t.Fatalf("second delivery must dedupe: created=%v err=%v", created, err)
	}
	delivered, err := s.BanDelivered("pool-abc12", "ep_1")
	if err != nil || !delivered {
		t.Fatalf("BanDelivered: %v %v", delivered, err)
	}
	runs, err := s.RunsByUser(u.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("dedupe must leave one run, got %d err=%v", len(runs), err)
	}
}

func TestUndeliveredBanEpisodesSkipHistoricalByDefault(t *testing.T) {
	s := openTest(t)
	u := mustUser(t, s, "historic", 5)
	pv := testGraphVersion(t, s, u.ID)
	if err := s.AddBanEpisode("pool-old", "203.0.113.1", "ep_old"); err != nil {
		t.Fatal(err)
	}
	// activation watermark in the future: historical episode is skipped.
	if _, err := s.SetNodeManagementProfile("pool-old", u.ID, 1, pv.ID, `{}`, false, 0, 9999999999); err != nil {
		t.Fatal(err)
	}
	cands, err := s.UndeliveredBanEpisodes()
	if err != nil || len(cands) != 0 {
		t.Fatalf("historical ban must not be delivered: %+v err=%v", cands, err)
	}
}

// C1/L1: operation-gated create inserts exactly one run and links the op id.
func TestCreateRunIdempotentGate(t *testing.T) {
	s := openTest(t)
	u := mustUser(t, s, "idemowner", 5)
	pv := testGraphVersion(t, s, u.ID)
	op := Operation{UserID: u.ID, Kind: "profile_run", State: OpPending, IdemKey: "op-key"}
	runIn := ProfileRun{UserID: u.ID, ProfileVersionID: pv.ID, ProfileID: pv.ProfileID, NodeID: "pool-abc12", Entry: "initial"}
	run, got, created, err := s.CreateRunIdempotent(op, runIn, "hash")
	if err != nil || !created || run.ID == 0 || run.OperationID != got.ID {
		t.Fatalf("first create: run=%+v op=%+v created=%v err=%v", run, got, created, err)
	}
	if _, _, created, err := s.CreateRunIdempotent(op, runIn, "hash"); err != nil || created {
		t.Fatalf("duplicate must not create a run: created=%v err=%v", created, err)
	}
	runs, err := s.RunsByUser(u.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("want one run, got %d err=%v", len(runs), err)
	}
	if runs[0].OperationID != got.ID {
		t.Fatalf("operation_id not persisted: %+v", runs[0])
	}
}

// H4: a profile pinned by an active policy cannot be deleted.
func TestDeleteProfileInUse(t *testing.T) {
	s := openTest(t)
	u := mustUser(t, s, "profileinuse", 5)
	pv := testGraphVersion(t, s, u.ID)
	if _, err := s.SetNodeManagementProfile("pool-inuse", u.ID, 1, pv.ID, `{}`, false, 0, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProfile(pv.ProfileID); !errors.Is(err, ErrProfileInUse) {
		t.Fatalf("want ErrProfileInUse, got %v", err)
	}

	// A pinned non-terminal run also blocks deletion.
	u2 := mustUser(t, s, "profileinuserun", 5)
	pv2 := testGraphVersion(t, s, u2.ID)
	if _, err := s.CreateRun(ProfileRun{
		UserID: u2.ID, ProfileVersionID: pv2.ID, ProfileID: pv2.ProfileID, Entry: "initial",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProfile(pv2.ProfileID); !errors.Is(err, ErrProfileInUse) {
		t.Fatalf("non-terminal run: want ErrProfileInUse, got %v", err)
	}
}

// H7: an unresolved run blocks a new ban delivery/purchase.
func TestBanDeliveryOneActiveRunPerPolicy(t *testing.T) {
	s := openTest(t)
	u := mustUser(t, s, "banactive", 5)
	pv := testGraphVersion(t, s, u.ID)
	if _, err := s.SetNodeManagementProfile("pool-active", u.ID, 1, pv.ID, `{}`, false, 0, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRun(ProfileRun{
		UserID: u.ID, ProfileVersionID: pv.ID, ProfileID: pv.ProfileID,
		NodeID: "pool-active", Entry: "observe", State: RunWaitingRetry,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddBanEpisode("pool-active", "203.0.113.77", "ep_active"); err != nil {
		t.Fatal(err)
	}
	cands, err := s.UndeliveredBanEpisodes()
	if err != nil || len(cands) != 1 {
		t.Fatalf("candidates: %+v err=%v", cands, err)
	}
	if _, created, err := s.CreateRunForBan(cands[0], ProfileRun{}); err != nil || created {
		t.Fatalf("active run must block a second delivery: created=%v err=%v", created, err)
	}
	runs, _ := s.RunsByUser(u.ID)
	if len(runs) != 1 {
		t.Fatalf("want one run, got %d", len(runs))
	}

	// A fresh policy populates policy_id on its delivered run.
	u2 := mustUser(t, s, "banactive2", 5)
	pv2 := testGraphVersion(t, s, u2.ID)
	if _, err := s.SetNodeManagementProfile("pool-active2", u2.ID, 1, pv2.ID, `{}`, false, 0, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.AddBanEpisode("pool-active2", "203.0.113.78", "ep_active2"); err != nil {
		t.Fatal(err)
	}
	cands, _ = s.UndeliveredBanEpisodes()
	for _, c := range cands {
		if c.Episode.NodeID != "pool-active2" {
			continue
		}
		got, created, err := s.CreateRunForBan(c, ProfileRun{})
		if err != nil || !created || got.PolicyID <= 0 {
			t.Fatalf("policy_id not populated: %+v created=%v err=%v", got, created, err)
		}
	}
}

// H8: a paid replacement (replaces_resource_id set, replaced_by_resource_id
// NULL, no active run) blocks a new ban delivery until it is deleted.
func TestBanDeliveryBlockedByUnfencedOrphan(t *testing.T) {
	s := openTest(t)
	u := mustUser(t, s, "banorphan", 5)
	pv := testGraphVersion(t, s, u.ID)
	conn := mustConn(t, s, u.ID, ProviderNodehost, "main", "sealed-1")

	current, err := s.CreateCloudResource(CloudResourceV2{
		UserID: u.ID, ConnectionID: conn.ID, Provider: ProviderNodehost, Label: "current",
		NodeID: "pool-orphan", State: CloudResReady,
	})
	if err != nil {
		t.Fatal(err)
	}
	orphan, err := s.CreateCloudResource(CloudResourceV2{
		UserID: u.ID, ConnectionID: conn.ID, Provider: ProviderNodehost, Label: "orphan",
		NodeID: "pool-orphan", State: CloudResReady, ReplacesResourceID: current.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetNodeManagementProfile("pool-orphan", u.ID, 1, pv.ID, `{}`, false, current.ID, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.AddBanEpisode("pool-orphan", "203.0.113.60", "ep_orphan"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.UnresolvedFencedResources(u.ID, "pool-orphan", current.ID); err != nil || n != 1 {
		t.Fatalf("unresolved: n=%d err=%v", n, err)
	}
	cands, err := s.UndeliveredBanEpisodes()
	if err != nil || len(cands) != 1 {
		t.Fatalf("candidates: %+v err=%v", cands, err)
	}
	if _, created, err := s.CreateRunForBan(cands[0], ProfileRun{}); err != nil || created {
		t.Fatalf("unfenced orphan must block delivery: created=%v err=%v", created, err)
	}
	if runs, _ := s.RunsByUser(u.ID); len(runs) != 0 {
		t.Fatalf("blocked delivery must not create a run, got %d", len(runs))
	}

	if err := s.UpdateCloudResourceState(orphan.ID, CloudResReady, CloudResDeleted, "", ""); err != nil {
		t.Fatal(err)
	}
	if n, err := s.UnresolvedFencedResources(u.ID, "pool-orphan", current.ID); err != nil || n != 0 {
		t.Fatalf("after delete: n=%d err=%v", n, err)
	}
	if _, created, err := s.CreateRunForBan(cands[0], ProfileRun{}); err != nil || !created {
		t.Fatalf("delete must release delivery: created=%v err=%v", created, err)
	}
}
