package server

// «Цена 1000 соединений»: сколько CPU (ядер), памяти и сети обходится прокси
// (плюс загрузка всего сервера в % — она есть у любых агентов, процесс
// прокси присылают только новые)
// на 1000 клиентских соединений — для сравнения прокси (classic, MEKO,
// MTProxyL, mtdd) на разных серверах. CPU и память — самого процесса прокси
// (агент находит его по слушающему сокету), не всего хоста: фон ОС и соседей
// сравнение не портит. Сеть — трафик прокси.
//
// Две оценки по точкам за окно (точка на ноду в минуту, load_samples):
//   - средняя: mean(ресурс) / mean(соединения) × 1000 — «во что в среднем
//     обходится», вместе с фоном процесса;
//   - предельная: наклон регрессии ресурс = фон + наклон × соединения,
//     × 1000 — сколько добавляет каждая тысяча соединений сверх фона. Дана,
//     только когда соединений было достаточно разных (иначе наклон — шум);
//     R² показывает, насколько линейная модель объясняет данные.

import (
	"math"
	"sort"
	"sync"
	"time"

	"sharedd/registry/internal/history"
)

const (
	connCostWindow    = 24 * time.Hour
	loadSampleEvery   = time.Minute
	connCostCacheTTL  = time.Minute
	connCostMinConns  = 50 // средняя цена на горстке соединений — шум фона
	connCostMinPoints = 30 // минимум точек для наклона
)

// costFit — оценка одного ресурса. nil-поля — посчитать честно нельзя.
type costFit struct {
	AvgPer1k      *float64 `json:"avg_per_1k"`
	MarginalPer1k *float64 `json:"marginal_per_1k"`
	Baseline      *float64 `json:"baseline"`
	R2            *float64 `json:"r2"`
	Points        int      `json:"points"`
}

type connCost struct {
	WindowHours int     `json:"window_hours"`
	Points      int     `json:"points"`
	ConnsMin    float64 `json:"conns_min"`
	ConnsMax    float64 `json:"conns_max"`
	ConnsAvg    float64 `json:"conns_avg"`
	CPU         costFit `json:"cpu_cores"`
	RAM         costFit `json:"ram_mb"`
	Net         costFit `json:"net_mbps"`
	// Загрузка всего сервера, % — по данным любых агентов. Сравнима между
	// нодами только на серверах одного размера.
	HostCPU costFit `json:"host_cpu_pct"`
	HostRAM costFit `json:"host_ram_pct"`
}

// fitCost — оценки по парам (соединения, ресурс), ресурс уже в единицах вывода.
func fitCost(xs, ys []float64) costFit {
	f := costFit{Points: len(xs)}
	n := float64(len(xs))
	if len(xs) == 0 {
		return f
	}
	var sx, sy float64
	for i := range xs {
		sx += xs[i]
		sy += ys[i]
	}
	mx, my := sx/n, sy/n
	if mx >= connCostMinConns {
		v := my / mx * 1000
		f.AvgPer1k = &v
	}
	if len(xs) < connCostMinPoints {
		return f
	}
	var sxx, sxy, syy float64
	for i := range xs {
		dx, dy := xs[i]-mx, ys[i]-my
		sxx += dx * dx
		sxy += dx * dy
		syy += dy * dy
	}
	// Наклон — только при заметном разбросе соединений.
	if std := math.Sqrt(sxx / n); std < math.Max(25, 0.05*mx) {
		return f
	}
	slope := sxy / sxx
	intercept := my - slope*mx
	m := slope * 1000
	f.MarginalPer1k, f.Baseline = &m, &intercept
	r2 := 1.0
	if syy > 0 {
		r2 = sxy * sxy / (sxx * syy)
	}
	f.R2 = &r2
	return f
}

// estimateConnCost — оценки по точкам одной ноды.
func estimateConnCost(samples []history.LoadSample) *connCost {
	if len(samples) == 0 {
		return nil
	}
	c := &connCost{WindowHours: int(connCostWindow.Hours()), Points: len(samples), ConnsMin: math.Inf(1)}
	var sum float64
	var cpuX, cpuY, ramX, ramY, netX, netY, hcX, hcY, hrX, hrY []float64
	for _, s := range samples {
		sum += s.Conns
		c.ConnsMin = math.Min(c.ConnsMin, s.Conns)
		c.ConnsMax = math.Max(c.ConnsMax, s.Conns)
		if s.CPUCores != nil {
			cpuX, cpuY = append(cpuX, s.Conns), append(cpuY, *s.CPUCores)
		}
		if s.RSSBytes != nil {
			ramX, ramY = append(ramX, s.Conns), append(ramY, *s.RSSBytes/(1<<20))
		}
		if s.NetBps != nil {
			netX, netY = append(netX, s.Conns), append(netY, *s.NetBps/1e6)
		}
		if s.HostCPUPct != nil {
			hcX, hcY = append(hcX, s.Conns), append(hcY, *s.HostCPUPct)
		}
		if s.HostRAMPct != nil {
			hrX, hrY = append(hrX, s.Conns), append(hrY, *s.HostRAMPct)
		}
	}
	c.ConnsAvg = sum / float64(len(samples))
	c.CPU, c.RAM, c.Net = fitCost(cpuX, cpuY), fitCost(ramX, ramY), fitCost(netX, netY)
	c.HostCPU, c.HostRAM = fitCost(hcX, hcY), fitCost(hrX, hrY)
	return c
}

// loadAccum — трафик ноды с прошлой точки (в памяти: после рестарта копится заново).
type loadAccum struct {
	bytes  int64
	since  time.Time
	lastAt time.Time
}

// recordLoadSampleLocked — точка «цены соединений» не чаще раза в минуту
// (под r.mu, из обработки metrics-отчёта).
func (r *Registry) recordLoadSampleLocked(nodeID string, snap map[string]float64, trafficBytes int64, now time.Time) {
	if r.db == nil {
		return
	}
	if r.loadAcc == nil {
		r.loadAcc = map[string]*loadAccum{}
	}
	acc := r.loadAcc[nodeID]
	if acc == nil {
		acc = &loadAccum{since: now}
		r.loadAcc[nodeID] = acc
	}
	acc.bytes += trafficBytes
	conns, ok := snap[userConnsMetric]
	if !ok || now.Sub(acc.lastAt) < loadSampleEvery {
		return
	}
	s := history.LoadSample{TS: now, NodeID: nodeID, Conns: conns}
	if v, ok := snap[proxyCPUCoresMetric]; ok {
		s.CPUCores = &v
	}
	if v, ok := snap[proxyRSSMetric]; ok {
		s.RSSBytes = &v
	}
	if v, ok := snap[hostCPUMetric]; ok {
		s.HostCPUPct = &v
	}
	if v, ok := snap[hostRAMMetric]; ok {
		s.HostRAMPct = &v
	}
	// Сеть — за полный интервал с прошлой точки (первая точка после старта
	// регистратора интервала не имеет). Агент без счётчиков трафика — «нет
	// данных», а не 0 Мбит/с.
	_, hasIn := snap[trafficIngressMetric]
	_, hasOut := snap[trafficEgressMetric]
	if !acc.lastAt.IsZero() && (hasIn || hasOut) {
		if secs := now.Sub(acc.since).Seconds(); secs > 0 {
			v := float64(acc.bytes) * 8 / secs
			s.NetBps = &v
		}
	}
	r.db.RecordLoadSample(s)
	acc.bytes, acc.since, acc.lastAt = 0, now, now
}

// connCostCache — оценки по всем нодам; публичная статистика поллится
// часто, а окно — сутки точек.
type connCostCache struct {
	mu     sync.Mutex
	at     time.Time
	byNode map[string]*connCost
}

func (r *Registry) connCosts(now time.Time) map[string]*connCost {
	r.costCache.mu.Lock()
	defer r.costCache.mu.Unlock()
	if r.costCache.byNode != nil && now.Sub(r.costCache.at) < connCostCacheTTL {
		return r.costCache.byNode
	}
	out := map[string]*connCost{}
	if r.db != nil {
		if rows, err := r.db.LoadSamplesSince(now.Add(-connCostWindow)); err == nil {
			for i := 0; i < len(rows); {
				j := i
				for j < len(rows) && rows[j].NodeID == rows[i].NodeID {
					j++
				}
				if c := estimateConnCost(rows[i:j]); c != nil {
					out[rows[i].NodeID] = c
				}
				i = j
			}
		}
	}
	r.costCache.byNode, r.costCache.at = out, now
	return out
}

// costByType — сравнение типов прокси: медианы оценок по нодам типа.
type costByType struct {
	NodeType string        `json:"node_type"`
	Nodes    int           `json:"nodes"`
	CPU      costTypeStats `json:"cpu_cores"`
	RAM      costTypeStats `json:"ram_mb"`
	Net      costTypeStats `json:"net_mbps"`
	HostCPU  costTypeStats `json:"host_cpu_pct"`
	HostRAM  costTypeStats `json:"host_ram_pct"`
}

type costTypeStats struct {
	AvgPer1k      *float64 `json:"avg_per_1k"`      // медиана средних по нодам
	AvgNodes      int      `json:"avg_nodes"`       // нод с оценкой
	MarginalPer1k *float64 `json:"marginal_per_1k"` // медиана предельных
	MarginalNodes int      `json:"marginal_nodes"`
}

func median(v []float64) *float64 {
	if len(v) == 0 {
		return nil
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	m := s[len(s)/2]
	if len(s)%2 == 0 {
		m = (s[len(s)/2-1] + s[len(s)/2]) / 2
	}
	return &m
}

func typeStats(fits []costFit) costTypeStats {
	var avg, marg []float64
	for _, f := range fits {
		if f.AvgPer1k != nil {
			avg = append(avg, *f.AvgPer1k)
		}
		if f.MarginalPer1k != nil {
			marg = append(marg, *f.MarginalPer1k)
		}
	}
	return costTypeStats{AvgPer1k: median(avg), AvgNodes: len(avg), MarginalPer1k: median(marg), MarginalNodes: len(marg)}
}

// buildCostByType — nodeTypes: node_id → тип ("" → "unknown").
func buildCostByType(costs map[string]*connCost, nodeTypes map[string]string) []costByType {
	group := map[string][]*connCost{}
	for id, c := range costs {
		t, ok := nodeTypes[id]
		if !ok {
			continue // нода уже не в пуле
		}
		if t == "" {
			t = "unknown"
		}
		group[t] = append(group[t], c)
	}
	out := make([]costByType, 0, len(group))
	for t, cs := range group {
		var cpu, ram, netF, hc, hr []costFit
		for _, c := range cs {
			cpu, ram, netF = append(cpu, c.CPU), append(ram, c.RAM), append(netF, c.Net)
			hc, hr = append(hc, c.HostCPU), append(hr, c.HostRAM)
		}
		out = append(out, costByType{NodeType: t, Nodes: len(cs), CPU: typeStats(cpu), RAM: typeStats(ram), Net: typeStats(netF),
			HostCPU: typeStats(hc), HostRAM: typeStats(hr)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeType < out[j].NodeType })
	return out
}
