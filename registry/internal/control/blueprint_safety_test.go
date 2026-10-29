package control

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestSafetyReplacementAncestry(t *testing.T) {
	s := openTest(t)
	u := mustUser(t, s, "safety-ancestry", 5)
	pv := testGraphVersion(t, s, u.ID)
	conn := mustConn(t, s, u.ID, ProviderNodehost, "main", "sealed")
	create := func(node string, parent int64) CloudResourceV2 {
		t.Helper()
		r, err := s.CreateCloudResource(CloudResourceV2{UserID: u.ID, ConnectionID: conn.ID,
			Provider: ProviderNodehost, NodeID: node, ReplacesResourceID: parent, State: CloudResReady})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	old := create("named", 0)
	if _, err := s.SetNodeManagementProfile("named", u.ID, 1, pv.ID, `{}`, false, old.ID, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.AddBanEpisode("named", "203.0.113.1", "safety-episode"); err != nil {
		t.Fatal(err)
	}
	candidates, err := s.UndeliveredBanEpisodes()
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates: %v %v", candidates, err)
	}
	for _, node := range []string{"", "different"} {
		t.Run(fmt.Sprintf("successor-%q", node), func(t *testing.T) {
			successor := create(node, old.ID)
			if n, err := s.UnresolvedFencedResources(u.ID, "named", old.ID); err != nil || n != 1 {
				t.Fatalf("guard: %d %v", n, err)
			}
			if _, created, err := s.CreateRunForBan(candidates[0], ProfileRun{}); err != nil || created {
				t.Fatalf("delivery: %v %v", created, err)
			}
			if n, err := s.UnresolvedFencedResources(u.ID, "unrelated", 0); err != nil || n != 0 {
				t.Fatalf("unrelated blocked: %d %v", n, err)
			}
			if n, err := s.UnresolvedFencedResources(u.ID+100, "named", old.ID); err != nil || n != 0 {
				t.Fatalf("foreign blocked: %d %v", n, err)
			}
			if err := s.UpdateCloudResourceState(successor.ID, CloudResReady, CloudResDeleted, "", ""); err != nil {
				t.Fatal(err)
			}
		})
	}
	// A policy may now point at a differently named successor while another
	// unclaimed sibling still refers only to their (deleted) common ancestor.
	ancestor := create("legacy", 0)
	current := create("new-name", ancestor.ID)
	create("", ancestor.ID)
	if err := s.UpdateCloudResourceState(ancestor.ID, CloudResReady, CloudResDeleted, "", ""); err != nil {
		t.Fatal(err)
	}
	if n, err := s.UnresolvedFencedResources(u.ID, "reverse-policy", current.ID); err != nil || n != 1 {
		t.Fatalf("reverse ancestry: %d %v", n, err)
	}
}

func TestSafetyNamedDeployment(t *testing.T) {
	s := openTest(t)
	u := mustUser(t, s, "safety-named", 5)
	pv := testGraphVersion(t, s, u.ID)
	conn := mustConn(t, s, u.ID, ProviderNodehost, "main", "sealed")
	create := func(node, key string) (ProfileRun, error) {
		r, _, _, err := s.CreateRunIdempotent(Operation{UserID: u.ID, Kind: "profile_run", State: OpPending, IdemKey: key},
			ProfileRun{UserID: u.ID, ProfileVersionID: pv.ID, ProfileID: pv.ProfileID, Entry: "initial", NodeID: node}, "hash")
		return r, err
	}
	run, err := create("named", "first")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := create("named", "active-duplicate"); !errors.Is(err, ErrDeploymentUnresolved) {
		t.Fatalf("active: %v", err)
	}
	if _, err := s.RequestRunCancel(run.ID, u.ID, run.Revision); err != nil {
		t.Fatal(err)
	}
	res, err := s.CreateCloudResource(CloudResourceV2{UserID: u.ID, ConnectionID: conn.ID, Provider: ProviderNodehost, State: CloudResPlanned})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateOperation(Operation{UserID: u.ID, Kind: OpKindCloudCreate, State: OpPending,
		IdemKey: fmt.Sprintf("bpcreate:%d:create", run.ID), RequestJSON: fmt.Sprintf(`{"run_id":%d,"resource_id":%d}`, run.ID, res.ID)}); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{CloudResPlanned, CloudResReady} {
		if state == CloudResReady {
			if err := s.UpdateCloudResourceState(res.ID, CloudResPlanned, state, "paid", "203.0.113.2"); err != nil {
				t.Fatal(err)
			}
			if err := s.SetCloudResourceNode(res.ID, "different", state); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := create("named", "duplicate-"+state); !errors.Is(err, ErrDeploymentUnresolved) {
			t.Fatalf("%s: %v", state, err)
		}
		if err := s.CheckNamedDeployment(u.ID, "named", run.ID); !errors.Is(err, ErrDeploymentUnresolved) {
			t.Fatalf("dispatch %s: %v", state, err)
		}
	}
	for i, node := range []string{"unrelated", "", ""} {
		if _, err := create(node, fmt.Sprintf("fresh-%d", i)); err != nil {
			t.Fatalf("fresh: %v", err)
		}
	}
	if err := s.UpdateCloudResourceState(res.ID, CloudResReady, CloudResDeleted, "", ""); err != nil {
		t.Fatal(err)
	}
	resolved, err := create("named", "resolved")
	if err != nil {
		t.Fatalf("resolved: %v", err)
	}
	resource := CloudResourceV2{UserID: u.ID, ConnectionID: conn.ID, Provider: ProviderNodehost}
	if _, err := s.CreateNamedDeploymentResource(resolved, resource); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := s.CreateNamedDeploymentResource(resolved, resource); !errors.Is(err, ErrDeploymentUnresolved) {
		t.Fatalf("repeat dispatch: %v", err)
	}
}

func TestSafetyRunCommandAtomic(t *testing.T) {
	for _, cancel := range []bool{true, false} {
		t.Run(fmt.Sprintf("cancel-%v", cancel), func(t *testing.T) {
			s := openTest(t)
			u := mustUser(t, s, "safety-command", 5)
			pv := testGraphVersion(t, s, u.ID)
			var runs []ProfileRun
			var cps []BlockCheckpoint
			for i := 0; i < 2; i++ {
				r, err := s.CreateRun(ProfileRun{UserID: u.ID, ProfileVersionID: pv.ID, ProfileID: pv.ProfileID, Entry: "initial", State: RunPaused})
				if err != nil {
					t.Fatal(err)
				}
				cp, err := s.SaveCheckpoint(BlockCheckpoint{RunID: r.ID, NodeID: "read", State: CheckpointWaiting, OutputJSON: "preserve", LeaseGeneration: 4})
				if err != nil {
					t.Fatal(err)
				}
				runs, cps = append(runs, r), append(cps, cp)
			}
			state, kind := RunWaitingRetry, "profile_run_resume"
			if cancel {
				state, kind = "", "profile_run_cancel"
			}
			apply := func(i int, key, hash string, revision int64) error {
				var cp *BlockCheckpoint
				if !cancel {
					cp = &cps[i]
				}
				_, _, _, err := s.ApplyRunCommand(Operation{UserID: u.ID, Kind: kind, IdemKey: key}, runs[i].ID, revision, hash, state, cp, !cancel)
				return err
			}
			type result struct {
				index int
				err   error
			}
			start, results := make(chan struct{}), make(chan result, 2)
			for i := 0; i < 2; i++ {
				go func(i int) { <-start; results <- result{i, apply(i, "shared", "one", 1)} }(i)
			}
			close(start)
			winnerIndex, wins, conflicts := 0, 0, 0
			for i := 0; i < 2; i++ {
				got := <-results
				if got.err == nil {
					winnerIndex = got.index
					wins++
				} else if errors.Is(got.err, ErrRunCommandConflict) {
					conflicts++
				} else {
					t.Fatalf("command: %v", got.err)
				}
			}
			if wins != 1 || conflicts != 1 {
				t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
			}
			if winnerIndex == 1 {
				runs[0], runs[1] = runs[1], runs[0]
				cps[0], cps[1] = cps[1], cps[0]
			}
			if err := apply(1, "stale", "one", 99); !errors.Is(err, ErrRevisionConflict) {
				t.Fatalf("revision: %v", err)
			}
			got, err := s.RunByID(runs[1].ID)
			if err != nil || !reflect.DeepEqual(got, runs[1]) {
				t.Fatalf("loser mutated: %+v %v", got, err)
			}
			cp, err := s.CheckpointByNode(runs[1].ID, "read", 0)
			if err != nil || !reflect.DeepEqual(cp, cps[1]) {
				t.Fatalf("checkpoint mutated: %+v %v", cp, err)
			}
			if _, err := s.OperationByIdemKey("stale"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("stale key not rolled back: %v", err)
			}
			if err := apply(0, "shared", "changed-body", 1); !errors.Is(err, ErrRunCommandConflict) {
				t.Fatalf("hash: %v", err)
			}
			if err := apply(0, "shared", "one", 1); err != nil {
				t.Fatalf("replay: %v", err)
			}
			winner, err := s.RunByID(runs[0].ID)
			if err != nil || winner.Revision != 2 {
				t.Fatalf("replay mutated revision: %d %v", winner.Revision, err)
			}
			cp, err = s.CheckpointByNode(runs[0].ID, "read", 0)
			if err != nil {
				t.Fatal(err)
			}
			if !cancel && (cp.State != CheckpointReady || cp.OutputJSON != "" || cp.LeaseGeneration != cps[0].LeaseGeneration+1) {
				t.Fatalf("winning reset not committed: %+v", cp)
			}
		})
	}
}

func TestSafetyRunCommandCheckpointRollback(t *testing.T) {
	for _, change := range []string{"state", "lease", "output", "write-error"} {
		t.Run(change, func(t *testing.T) {
			s := openTest(t)
			u := mustUser(t, s, "safety-checkpoint", 5)
			pv := testGraphVersion(t, s, u.ID)
			run, err := s.CreateRun(ProfileRun{UserID: u.ID, ProfileVersionID: pv.ID, ProfileID: pv.ProfileID, Entry: "initial", State: RunPaused})
			if err != nil {
				t.Fatal(err)
			}
			cp, err := s.SaveCheckpoint(BlockCheckpoint{RunID: run.ID, NodeID: "read", State: CheckpointWaiting, OutputJSON: "preserve", LeaseGeneration: 4})
			if err != nil {
				t.Fatal(err)
			}
			current := cp
			switch change {
			case "state":
				current.State = CheckpointDispatched
			case "lease":
				current.LeaseGeneration++
			case "output":
				current.OutputJSON = "new outcome"
			case "write-error":
				if _, err := s.sql.Exec(`CREATE TRIGGER fail_checkpoint BEFORE UPDATE ON block_checkpoints BEGIN SELECT RAISE(ABORT, 'injected checkpoint error'); END`); err != nil {
					t.Fatal(err)
				}
			}
			if change != "write-error" {
				current, err = s.SaveCheckpoint(current)
				if err != nil {
					t.Fatal(err)
				}
			}
			_, _, _, err = s.ApplyRunCommand(Operation{UserID: u.ID, Kind: "profile_run_resume", IdemKey: "resume"}, run.ID, run.Revision, "hash", RunWaitingRetry, &cp, true)
			if err == nil || (change != "write-error" && !errors.Is(err, ErrRevisionConflict)) {
				t.Fatalf("conflict: %v", err)
			}
			got, err := s.RunByID(run.ID)
			if err != nil || got != run {
				t.Fatalf("run mutated: %+v %v", got, err)
			}
			gotCP, err := s.CheckpointByNode(run.ID, cp.NodeID, cp.Invocation)
			if err != nil || gotCP != current {
				t.Fatalf("checkpoint mutated: %+v %v", gotCP, err)
			}
			if _, err := s.OperationByIdemKey("resume"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("key survived rollback: %v", err)
			}
		})
	}
}
