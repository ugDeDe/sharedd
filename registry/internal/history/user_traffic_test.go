package history

import (
	"testing"
	"time"
)

func TestUserTrafficRoundTrip(t *testing.T) {
	d := Open(t.TempDir() + "/hist.db")
	if d == nil {
		t.Fatal("history db failed to open")
	}
	defer d.Close()
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	d.RecordUserTraffic(base, "n1", "alice", 100, 900)
	d.RecordUserTraffic(base.Add(time.Minute), "n1", "alice", 50, 400)
	d.RecordUserTraffic(base, "n2", "bob", 7, 8)

	rows, err := d.UserTrafficSince("n1", base.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 n1 rows, got %d (%v)", len(rows), rows)
	}
	if rows[0].User != "alice" || rows[0].Ingress != 100 || rows[0].Egress != 900 {
		t.Fatalf("first row mismatch: %+v", rows[0])
	}
	if rows[1].Ingress != 50 || rows[1].Egress != 400 {
		t.Fatalf("second row mismatch: %+v", rows[1])
	}

	if rows, _ := d.UserTrafficSince("n2", base.Add(-time.Hour)); len(rows) != 1 || rows[0].User != "bob" {
		t.Fatalf("node filter broken: %v", rows)
	}
	if rows, _ := d.UserTrafficSince("n1", base.Add(time.Second)); len(rows) != 1 {
		t.Fatalf("since filter broken: %v", rows)
	}
}

func TestUserTrafficRetention(t *testing.T) {
	d := Open(t.TempDir() + "/hist.db")
	if d == nil {
		t.Fatal("history db failed to open")
	}
	defer d.Close()
	d.RecordUserTraffic(time.Now().Add(-40*24*time.Hour), "n1", "alice", 1, 1)
	d.RecordUserTraffic(time.Now(), "n1", "alice", 2, 2)

	d.PruneEvents(time.Now())

	rows, err := d.UserTrafficSince("n1", time.Now().Add(-100*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("row older than 35d must be purged, got %d rows", len(rows))
	}
}
