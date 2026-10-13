package history

import (
	"testing"
	"time"
)

func TestHostMetricsRoundTrip(t *testing.T) {
	d := Open(t.TempDir() + "/hist.db")
	if d == nil {
		t.Fatal("history db failed to open")
	}
	defer d.Close()
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cpu, ram := 42.5, 88.0
	d.RecordHostMetrics(base, "n1", &cpu, &ram)
	d.RecordHostMetrics(base.Add(time.Minute), "n1", &cpu, nil)
	d.RecordHostMetrics(base, "n2", nil, &ram)
	d.RecordHostMetrics(base, "n1", nil, nil) // обе метрики отсутствуют — строки нет

	rows, err := d.HostMetricsSince("n1", base.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 n1 rows, got %d (%+v)", len(rows), rows)
	}
	if rows[0].CPUPercent == nil || *rows[0].CPUPercent != 42.5 || rows[0].RAMPercent == nil || *rows[0].RAMPercent != 88.0 {
		t.Fatalf("first row mismatch: %+v", rows[0])
	}
	if rows[1].CPUPercent == nil || *rows[1].CPUPercent != 42.5 || rows[1].RAMPercent != nil {
		t.Fatalf("second row must carry cpu only: %+v", rows[1])
	}

	if rows, _ := d.HostMetricsSince("n2", base.Add(-time.Hour)); len(rows) != 1 || rows[0].CPUPercent != nil || rows[0].RAMPercent == nil {
		t.Fatalf("node filter / nullable columns broken: %+v", rows)
	}
	if rows, _ := d.HostMetricsSince("n1", base.Add(time.Second)); len(rows) != 1 {
		t.Fatalf("since filter broken: %v", rows)
	}
}

func TestHostMetricsRetention(t *testing.T) {
	d := Open(t.TempDir() + "/hist.db")
	if d == nil {
		t.Fatal("history db failed to open")
	}
	defer d.Close()
	old, fresh := 10.0, 20.0
	d.RecordHostMetrics(time.Now().Add(-40*24*time.Hour), "n1", &old, nil)
	d.RecordHostMetrics(time.Now(), "n1", &fresh, nil)

	d.PruneEvents(time.Now())

	rows, err := d.HostMetricsSince("n1", time.Now().Add(-100*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].CPUPercent == nil || *rows[0].CPUPercent != fresh {
		t.Fatalf("row older than 35d must be purged, got %+v", rows)
	}
}
