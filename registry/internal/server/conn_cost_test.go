package server

import (
	"math"
	"testing"
	"time"

	"sharedd/registry/internal/history"
)

func near(t *testing.T, name string, got *float64, want, tol float64) {
	t.Helper()
	if got == nil || math.Abs(*got-want) > tol {
		t.Fatalf("%s: got %v, want %.4f±%.4f", name, got, want, tol)
	}
}

// Ресурс = фон + наклон × соединения восстанавливается; средняя включает фон.
func TestFitCostRecoversMarginalAndBaseline(t *testing.T) {
	var xs, ys []float64
	for i := 0; i < 120; i++ {
		x := 500 + float64(i%60)*50 // 500..3450 соединений
		noise := 0.002 * math.Sin(float64(i))
		xs, ys = append(xs, x), append(ys, 0.05+0.0002*x+noise)
	}
	f := fitCost(xs, ys)
	near(t, "marginal", f.MarginalPer1k, 0.2, 0.01)
	near(t, "baseline", f.Baseline, 0.05, 0.01)
	if f.R2 == nil || *f.R2 < 0.98 {
		t.Fatalf("r2 %v", f.R2)
	}
	var sx, sy float64
	for i := range xs {
		sx, sy = sx+xs[i], sy+ys[i]
	}
	near(t, "avg", f.AvgPer1k, sy/sx*1000, 1e-9)
}

func TestFitCostRefusesNoise(t *testing.T) {
	var xs, ys []float64
	for i := 0; i < 100; i++ { // соединений почти не меняется — наклон не определён
		xs, ys = append(xs, 1000+float64(i%3)), append(ys, 0.3+0.01*math.Sin(float64(i)))
	}
	if f := fitCost(xs, ys); f.MarginalPer1k != nil || f.AvgPer1k == nil {
		t.Fatalf("flat conns: marginal must be absent, avg present: %+v", f)
	}
	if f := fitCost(xs[:10], ys[:10]); f.MarginalPer1k != nil {
		t.Fatal("too few points for a slope")
	}
	if f := fitCost([]float64{5, 6}, []float64{0.1, 0.1}); f.AvgPer1k != nil {
		t.Fatal("average over a handful of connections is background noise")
	}
}

func TestRecordLoadSampleOncePerMinuteWithNetRate(t *testing.T) {
	r := newTestRegistry(t)
	r.db = openHistoryDB(t.TempDir() + "/hist.db")
	now := time.Unix(1_800_000_000, 0)
	snap := map[string]float64{userConnsMetric: 1200, proxyCPUCoresMetric: 0.4, proxyRSSMetric: 300 << 20, hostCPUMetric: 60,
		trafficIngressMetric: 1, trafficEgressMetric: 1}
	r.recordLoadSampleLocked("n1", snap, 0, now)                                    // первая точка: сети нет
	r.recordLoadSampleLocked("n1", snap, 3_000_000, now.Add(30*time.Second))        // рано
	r.recordLoadSampleLocked("n1", snap, 4_500_000, now.Add(60*time.Second))        // 7.5 МБ за 60 с
	r.recordLoadSampleLocked("n1", map[string]float64{}, 0, now.Add(3*time.Minute)) // нет соединений
	rows, err := r.db.LoadSamplesSince(now.Add(-time.Hour))
	if err != nil || len(rows) != 2 {
		t.Fatalf("want 2 samples, got %d %v", len(rows), err)
	}
	if rows[0].NetBps != nil || rows[1].NetBps == nil || math.Abs(*rows[1].NetBps-1_000_000) > 1 {
		t.Fatalf("net rate: first=%v second=%v (want 1 Mbit/s)", rows[0].NetBps, rows[1].NetBps)
	}
	// Агент без счётчиков трафика: сеть — «нет данных», не 0.
	r.recordLoadSampleLocked("n2", map[string]float64{userConnsMetric: 900}, 0, now)
	r.recordLoadSampleLocked("n2", map[string]float64{userConnsMetric: 900}, 0, now.Add(time.Minute))
	if all, _ := r.db.LoadSamplesSince(now.Add(-time.Hour)); len(all) != 4 || all[3].NetBps != nil {
		t.Fatalf("node without traffic counters must have no net rate: %+v", all[len(all)-1])
	}
	c := estimateConnCost(rows)
	near(t, "ram avg", c.RAM.AvgPer1k, 250, 0.01)         // 300 МиБ на 1200 соединений
	near(t, "host cpu avg", c.HostCPU.AvgPer1k, 50, 0.01) // 60% сервера на 1200 соединений
	if c.HostRAM.Points != 0 {
		t.Fatal("no host RAM metric — no host RAM points")
	}
}

func TestCostByTypeMedians(t *testing.T) {
	f := func(cpu float64) *connCost {
		v := cpu
		return &connCost{CPU: costFit{AvgPer1k: &v}}
	}
	costs := map[string]*connCost{"a": f(0.1), "b": f(0.3), "c": f(0.2), "d": f(0.9), "gone": f(5)}
	types := map[string]string{"a": "mtdd", "b": "mtdd", "c": "mtdd", "d": "", "x": "mtdd"}
	out := buildCostByType(costs, types)
	if len(out) != 2 || out[0].NodeType != "mtdd" || out[1].NodeType != "unknown" {
		t.Fatalf("groups: %+v", out)
	}
	if out[0].Nodes != 3 || out[0].CPU.AvgNodes != 3 {
		t.Fatalf("mtdd counts: %+v", out[0])
	}
	near(t, "mtdd median", out[0].CPU.AvgPer1k, 0.2, 1e-9)
}

func TestLoadSamplesRoundTrip(t *testing.T) {
	d := history.Open(t.TempDir() + "/h.db")
	if d == nil {
		t.Fatal("open")
	}
	defer d.Close()
	now := time.Now()
	v := 0.5
	d.RecordLoadSample(history.LoadSample{TS: now, NodeID: "n1", Conns: 10, CPUCores: &v})
	rows, err := d.LoadSamplesSince(now.Add(-time.Minute))
	if err != nil || len(rows) != 1 || rows[0].CPUCores == nil || *rows[0].CPUCores != 0.5 || rows[0].RSSBytes != nil {
		t.Fatalf("round trip: %+v %v", rows, err)
	}
}
