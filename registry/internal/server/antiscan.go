package server

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"sharedd/registry/internal/antiscan"
	"sharedd/registry/internal/config"
	"sharedd/registry/internal/machineapi"
)

// Антискан: регистратор — единственный владелец СПИСКА сканеров, агент —
// владелец его ПРИМЕНЕНИЯ (ipset/iptables на общем порту). Раз в
// [antiscan] interval_min скачиваются abuse-листы, от них остаются только
// адреса из российских диапазонов (geo_url), результат сворачивается в CIDR
// и раздаётся агентам: /config несёт ревизию, сам список — GET /antiscan.
//
// Отказ источника не обнуляет список: используется последний удачный ответ
// этого источника. Без гео-набора список не публикуется вовсе (нефильтрованный
// abuse-лист нодам не уходит). После рестарта регистратора до первой сборки
// ревизии нет — агенты держат последний применённый set.

const (
	antiscanFetchTimeout = 60 * time.Second
	antiscanMaxBody      = 64 << 20
	// RIPE-выделения меняются редко; гео-набор перекачивается раз в сутки
	// (или сразу при смене geo_url).
	antiscanGeoRefresh = 24 * time.Hour
)

type antiscanSourceState struct {
	set       antiscan.Set
	entries   int
	fetchedAt time.Time
	err       string
}

type antiscanRuntime struct {
	mu          sync.RWMutex
	revision    string
	body        []byte
	entries     int
	builtAt     time.Time
	lastAttempt time.Time
	lastErr     string
	sources     map[string]*antiscanSourceState
	geo         antiscan.Set
	geoURL      string
	geoAt       time.Time
	geoErr      string

	// kick — внеочередная пересборка (смена настроек в панели). nil в тестах,
	// собирающих Registry литералом: select с default это переживает.
	kick chan struct{}
	// fetch — тестовый шов; nil → HTTP GET.
	fetch func(ctx context.Context, url string) ([]byte, error)
}

func (r *Registry) kickAntiscan() {
	select {
	case r.scan.kick <- struct{}{}:
	default:
	}
}

func antiscanHTTPGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "sharedd-registry")
	resp, err := (&http.Client{Timeout: antiscanFetchTimeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, antiscanMaxBody+1))
	if err != nil {
		return nil, err
	}
	if len(data) > antiscanMaxBody {
		return nil, fmt.Errorf("response exceeds %d bytes", antiscanMaxBody)
	}
	return data, nil
}

func (r *Registry) antiscanLoop(ctx context.Context) {
	for {
		r.refreshAntiscan(ctx, time.Now())
		r.cfgMu.RLock()
		interval := time.Duration(r.cfg.Antiscan.IntervalMin) * time.Minute
		r.cfgMu.RUnlock()
		if interval <= 0 {
			interval = config.DefaultAntiscanIntervalMin * time.Minute
		}
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		case <-r.scan.kick:
			t.Stop()
		}
	}
}

// refreshAntiscan — одна сборка. Сеть — вне r.scan.mu: панель и агенты
// читают предыдущую ревизию, пока идут загрузки.
func (r *Registry) refreshAntiscan(ctx context.Context, now time.Time) {
	r.cfgMu.RLock()
	enabled := config.ResolveAntiscanEnabled(r.cfg.Antiscan.Enabled)
	sources := append([]string(nil), r.cfg.Antiscan.Sources...)
	geoURL := r.cfg.Antiscan.GeoURL
	r.cfgMu.RUnlock()
	if len(sources) == 0 {
		sources = []string{config.DefaultAntiscanSource}
	}
	if geoURL == "" {
		geoURL = config.DefaultAntiscanGeoURL
	}
	fetch := r.scan.fetch
	if fetch == nil {
		fetch = antiscanHTTPGet
	}

	if !enabled {
		r.scan.mu.Lock()
		if r.scan.revision != machineapi.AntiscanDisabled {
			log.Printf("antiscan: disabled in config — agents will drop the scanner set")
		}
		r.scan.revision, r.scan.body, r.scan.entries = machineapi.AntiscanDisabled, nil, 0
		r.scan.lastAttempt, r.scan.lastErr = now, ""
		r.scan.mu.Unlock()
		return
	}

	r.scan.mu.RLock()
	needGeo := r.scan.geo == nil || r.scan.geoURL != geoURL || now.Sub(r.scan.geoAt) >= antiscanGeoRefresh
	r.scan.mu.RUnlock()
	var (
		geo    antiscan.Set
		geoErr error
	)
	if needGeo {
		data, err := fetch(ctx, geoURL)
		if err == nil {
			geo, err = antiscan.ParseGeo(data)
		}
		geoErr = err
	}
	type result struct {
		set antiscan.Set
		err error
	}
	fetched := make(map[string]result, len(sources))
	for _, src := range sources {
		data, err := fetch(ctx, src)
		if err != nil {
			fetched[src] = result{err: err}
			continue
		}
		fetched[src] = result{set: antiscan.ParseList(data)}
	}

	r.scan.mu.Lock()
	defer r.scan.mu.Unlock()
	r.scan.lastAttempt = now
	if needGeo {
		if geoErr != nil {
			r.scan.geoErr = geoErr.Error()
			log.Printf("antiscan: geo list %s: %v", geoURL, geoErr)
			if r.scan.geoURL != geoURL {
				// Старый набор принадлежит другому URL — фильтровать им нельзя.
				r.scan.geo = nil
			}
		} else {
			r.scan.geo, r.scan.geoURL, r.scan.geoAt, r.scan.geoErr = geo, geoURL, now, ""
		}
	}
	if r.scan.sources == nil {
		r.scan.sources = map[string]*antiscanSourceState{}
	}
	keep := make(map[string]bool, len(sources))
	var union []antiscan.Set
	var errs []string
	for _, src := range sources {
		keep[src] = true
		st := r.scan.sources[src]
		if st == nil {
			st = &antiscanSourceState{}
			r.scan.sources[src] = st
		}
		if res := fetched[src]; res.err != nil {
			st.err = res.err.Error()
			errs = append(errs, src+": "+st.err)
			log.Printf("antiscan: source %s: %v (keeping last good copy)", src, res.err)
		} else {
			st.set, st.entries, st.fetchedAt, st.err = res.set, len(res.set.CIDRs()), now, ""
		}
		if !st.fetchedAt.IsZero() {
			union = append(union, st.set)
		}
	}
	for src := range r.scan.sources {
		if !keep[src] {
			delete(r.scan.sources, src)
		}
	}
	if r.scan.geo == nil {
		r.scan.lastErr = "нет гео-набора RU: " + r.scan.geoErr
		return
	}
	if len(union) == 0 {
		r.scan.lastErr = "ни один источник ещё не скачан: " + strings.Join(errs, "; ")
		return
	}
	entries := antiscan.Intersect(antiscan.Union(union...), r.scan.geo).CIDRs()
	if len(entries) > antiscan.MaxEntries {
		r.scan.lastErr = fmt.Sprintf("список из %d записей превышает лимит %d — оставлен предыдущий", len(entries), antiscan.MaxEntries)
		return
	}
	r.scan.lastErr = strings.Join(errs, "; ")
	rev := antiscan.Revision(entries)
	if rev == r.scan.revision {
		return
	}
	var body strings.Builder
	for _, e := range entries {
		body.WriteString(e)
		body.WriteByte('\n')
	}
	r.scan.revision, r.scan.body, r.scan.entries, r.scan.builtAt = rev, []byte(body.String()), len(entries), now
	log.Printf("antiscan: list rebuilt — %d RU entries, revision %s", len(entries), rev)
}

// antiscanRevision — для /config: "" — списка нет (агент держит последний),
// AntiscanDisabled — снять, иначе — ревизия содержимого.
func (r *Registry) antiscanRevision() string {
	r.scan.mu.RLock()
	defer r.scan.mu.RUnlock()
	return r.scan.revision
}

// handleAntiscanList — GET /antiscan: CIDR/адрес на строку. 204 — отдавать
// нечего (ещё не собран или выключен: агент ориентируется на ревизию в /config).
func (r *Registry) handleAntiscanList(w http.ResponseWriter, req *http.Request) {
	if !r.gateMachineAuth(w, req, req.Header.Get("X-ShareDD-Node-ID"), "") {
		return
	}
	r.scan.mu.RLock()
	rev, body := r.scan.revision, r.scan.body
	r.scan.mu.RUnlock()
	w.Header().Set("Cache-Control", "no-store")
	if rev == "" || rev == machineapi.AntiscanDisabled {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set(machineapi.AntiscanRevisionHeader, rev)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(body)
}

type antiscanSourceStatus struct {
	URL       string `json:"url"`
	Entries   int    `json:"entries"`
	FetchedAt string `json:"fetched_at,omitempty"`
	Error     string `json:"error,omitempty"`
}

type antiscanStatus struct {
	Revision    string                 `json:"revision"`
	Entries     int                    `json:"entries"`
	BuiltAt     string                 `json:"built_at,omitempty"`
	LastAttempt string                 `json:"last_attempt,omitempty"`
	LastError   string                 `json:"last_error,omitempty"`
	GeoRanges   int                    `json:"geo_ranges"`
	GeoAt       string                 `json:"geo_at,omitempty"`
	GeoError    string                 `json:"geo_error,omitempty"`
	Sources     []antiscanSourceStatus `json:"sources"`
}

func rfc3339OrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// antiscanStatusSnapshot — состояние сборки для панели (порядок источников —
// как в конфиге).
func (r *Registry) antiscanStatusSnapshot(sources []string) antiscanStatus {
	r.scan.mu.RLock()
	defer r.scan.mu.RUnlock()
	st := antiscanStatus{
		Revision:    r.scan.revision,
		Entries:     r.scan.entries,
		BuiltAt:     rfc3339OrEmpty(r.scan.builtAt),
		LastAttempt: rfc3339OrEmpty(r.scan.lastAttempt),
		LastError:   r.scan.lastErr,
		GeoRanges:   len(r.scan.geo),
		GeoAt:       rfc3339OrEmpty(r.scan.geoAt),
		GeoError:    r.scan.geoErr,
		Sources:     []antiscanSourceStatus{},
	}
	for _, src := range sources {
		s := antiscanSourceStatus{URL: src}
		if cur := r.scan.sources[src]; cur != nil {
			s.Entries, s.FetchedAt, s.Error = cur.entries, rfc3339OrEmpty(cur.fetchedAt), cur.err
		}
		st.Sources = append(st.Sources, s)
	}
	return st
}
