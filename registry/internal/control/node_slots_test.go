package control

import (
	"errors"
	"sync"
	"testing"
)

func TestNodeSlotsConcurrentUnique(t *testing.T) {
	s := openTest(t)
	u := mustUser(t, s, "slot-user", 20)
	const n = 8
	nums := make([]int64, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := &Store{sql: s.sql}
			tx, err := s.sql.Begin()
			if err != nil {
				t.Error(err)
				return
			}
			slot, err := ensureSlotTx(tx, u.ID, "n")
			if err != nil {
				_ = tx.Rollback()
				t.Error(err)
				return
			}
			if err := tx.Commit(); err != nil {
				t.Error(err)
				return
			}
			nums[i] = slot.Number
		}(i)
	}
	wg.Wait()
	seen := map[int64]bool{}
	for _, num := range nums {
		if num <= 0 || seen[num] {
			t.Fatalf("duplicate/invalid slot number %d in %v", num, nums)
		}
		seen[num] = true
	}
}

func TestAuthorizeNodeReusesSlot(t *testing.T) {
	s := openTest(t)
	u := mustUser(t, s, "reuse-user", 5)
	const nodeID = "alpha-aaaaa"
	if err := s.AuthorizeNode(nodeID, u); err != nil {
		t.Fatalf("first authorize: %v", err)
	}
	slot, ok, err := s.NodeSlotForNode(nodeID)
	if err != nil || !ok {
		t.Fatalf("slot after first authorize: ok=%v err=%v", ok, err)
	}
	if err := s.AuthorizeNode(nodeID, u); err != nil {
		t.Fatalf("second authorize: %v", err)
	}
	again, ok, err := s.NodeSlotForNode(nodeID)
	if err != nil || !ok || again.ID != slot.ID || again.Number != slot.Number {
		t.Fatalf("slot changed on repeat authorize: %+v vs %+v err=%v", again, slot, err)
	}
}

func TestBackfillNodeSlotsIdempotent(t *testing.T) {
	s := openTest(t)
	u := mustUser(t, s, "backfill-user", 5)
	if _, err := s.sql.Exec(`INSERT INTO node_owners (node_id, user_id, registered_at, legacy)
		VALUES ('one-aaaaa', ?, 10, 0), ('two-bbbbb', ?, 20, 0)`, u.ID, u.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := s.BackfillNodeSlots(); err != nil || n != 2 {
		t.Fatalf("backfill: n=%d err=%v", n, err)
	}
	s1, _, err := s.NodeSlotForNode("one-aaaaa")
	if err != nil {
		t.Fatal(err)
	}
	s2, _, err := s.NodeSlotForNode("two-bbbbb")
	if err != nil {
		t.Fatal(err)
	}
	if s1.Number == s2.Number || s1.Number <= 0 || s2.Number <= 0 {
		t.Fatalf("expected distinct positive numbers, got %d and %d", s1.Number, s2.Number)
	}
	if n, err := s.BackfillNodeSlots(); err != nil || n != 0 {
		t.Fatalf("second backfill must be a no-op: n=%d err=%v", n, err)
	}
	again, _, err := s.NodeSlotForNode("one-aaaaa")
	if err != nil || again.ID != s1.ID || again.Number != s1.Number {
		t.Fatalf("backfill renumbered existing slot: %+v vs %+v err=%v", again, s1, err)
	}
}

func TestRenameNodeSlotKeepsIdentity(t *testing.T) {
	s := openTest(t)
	u := mustUser(t, s, "rename-user", 5)
	const nodeID = "orig-aaaaa"
	if err := s.AuthorizeNode(nodeID, u); err != nil {
		t.Fatal(err)
	}
	slot, ok, err := s.NodeSlotForNode(nodeID)
	if err != nil || !ok {
		t.Fatalf("slot: ok=%v err=%v", ok, err)
	}
	updated, err := s.RenameNodeSlot(u.ID, slot.ID, "renamed", slot.Revision)
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if updated.Name != "renamed" || updated.Revision != slot.Revision+1 || updated.Number != slot.Number {
		t.Fatalf("rename result: %+v", updated)
	}
	if _, err := s.RenameNodeSlot(u.ID, slot.ID, "again", slot.Revision); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale revision: want ErrRevisionConflict, got %v", err)
	}
	if _, ok, err := s.NodeSlotForNode(nodeID); err != nil || !ok {
		t.Fatalf("technical identity lost after rename: ok=%v err=%v", ok, err)
	}
	other := mustUser(t, s, "other-user", 5)
	if _, err := s.RenameNodeSlot(other.ID, slot.ID, "hijack", updated.Revision); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign rename: want ErrNotFound, got %v", err)
	}
}

func TestLegacyUserDoesNotGetSlot(t *testing.T) {
	s := openTest(t)
	u, err := s.User(s.LegacyUserID())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AuthorizeNode("general-aaaaa", u.User); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.NodeSlotForNode("general-aaaaa"); err != nil || ok {
		t.Fatalf("legacy/shared node must not get a public slot: ok=%v err=%v", ok, err)
	}
}

func TestPublicNodeIdentitiesSkipsLegacy(t *testing.T) {
	s := openTest(t)
	u := mustUser(t, s, "public-user", 5)
	if err := s.AuthorizeNode("public-aaaaa", u); err != nil {
		t.Fatal(err)
	}
	if err := s.BindLegacyNode("legacy-bbbbb"); err != nil {
		t.Fatal(err)
	}
	got, err := s.PublicNodeIdentities([]string{"public-aaaaa", "legacy-bbbbb"})
	if err != nil {
		t.Fatal(err)
	}
	p, ok := got["public-aaaaa"]
	if !ok || p.Username != "public-user" || p.OwnerNodeNumber <= 0 {
		t.Fatalf("owner identity: %+v ok=%v", p, ok)
	}
	if p.DisplayName() != "public-user · Нода №1" {
		t.Fatalf("display name: %q", p.DisplayName())
	}
	if _, ok := got["legacy-bbbbb"]; ok {
		t.Fatal("legacy node must not get an owner identity")
	}
}
