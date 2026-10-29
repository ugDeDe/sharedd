package server

// Каталог + оценка цены hosting-провайдера (docs/specs/hosting-blueprints/
// implementation-stage-05-cloud-management.md): реализовано только для
// nodehost (проверено вживую 2026-10-17); beget → 501 not_implemented
// (legacy-wizard остаётся). Ответы каталoга собираются из нескольких
// GET-вызовов; ошибки провайдера → 502/504 как в verify-обработчике.

import (
	"context"
	"log"
	"net/http"
	"time"
	"unicode/utf8"

	"sharedd/registry/internal/control"
	"sharedd/registry/internal/nodehost"
)

// hostingCataloger — поверхность каталога/оценки; реализует только
// hostingNodehost, поэтому beget автоматически отваливается в 501.
type hostingCataloger interface {
	Catalog(ctx context.Context, location string) (hostingCatalogData, error)
	Estimate(ctx context.Context, req nodehost.EstimateRequest) (*nodehost.Estimate, error)
}

// hostingCatalogData — сырье каталога в типах провайдера; DTO собирает
// обработчик (нормализация id/os/disk/limits там же).
type hostingCatalogData struct {
	Locations []nodehost.Location
	OS        []nodehost.OSTemplate
	CPU       []string
	Disks     []nodehost.DiskType
	Limits    *nodehost.Limits
	Network   *nodehost.NetworkOptions // nil = ?location= не передан
}

// Catalog — пять GET-вызовов; network запрашивается только при location,
// его ошибка не валит весь каталог (недоступная сеть → available:false),
// ошибки остальных вызовов — провайдер-ошибка каталога.
func (p hostingNodehost) Catalog(ctx context.Context, location string) (hostingCatalogData, error) {
	cl := p.cl
	locs, err := cl.Locations(ctx)
	if err != nil {
		return hostingCatalogData{}, err
	}
	os, err := cl.OSTemplates(ctx)
	if err != nil {
		return hostingCatalogData{}, err
	}
	cpus, err := cl.CPUModels(ctx, location)
	if err != nil {
		return hostingCatalogData{}, err
	}
	disks, err := cl.DiskTypes(ctx)
	if err != nil {
		return hostingCatalogData{}, err
	}
	lim, err := cl.Limits(ctx)
	if err != nil {
		return hostingCatalogData{}, err
	}
	var net *nodehost.NetworkOptions
	if location != "" {
		// ponytail: любая ошибка network (в т.ч. неверная локация) деградирует
		// в available:false; отдельный 502 для network не нужен фронту.
		if no, nerr := cl.NetworkOptions(ctx, location); nerr == nil {
			net = no
		}
	}
	return hostingCatalogData{Locations: locs, OS: os, CPU: cpus, Disks: disks, Limits: lim, Network: net}, nil
}

func (p hostingNodehost) Estimate(ctx context.Context, req nodehost.EstimateRequest) (*nodehost.Estimate, error) {
	return p.cl.Estimate(ctx, req)
}

// ── DTO ───────────────────────────────────────────────────────────

type catalogOptionJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// catalogLocationJSON — локация идёт кодом ("code"), не id.
type catalogLocationJSON struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

type catalogRangeJSON struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

type catalogLimitsJSON struct {
	Cores  catalogRangeJSON `json:"cores"`
	RamGB  catalogRangeJSON `json:"ram_gb"`
	DiskGB catalogRangeJSON `json:"disk_gb"`
	Types  map[string]bool  `json:"types"`
}

type catalogNetworkJSON struct {
	Speeds    []string `json:"speeds"`
	Available bool     `json:"available"`
}

type hostingCatalogJSON struct {
	Provider  string                `json:"provider"`
	CheckedAt int64                 `json:"checked_at"`
	Locations []catalogLocationJSON `json:"locations"`
	OS        []catalogOptionJSON   `json:"os"`
	CPUModels []string              `json:"cpu_models"`
	DiskTypes []catalogOptionJSON   `json:"disk_types"`
	Limits    catalogLimitsJSON     `json:"limits"`
	Network   *catalogNetworkJSON   `json:"network,omitempty"`
}

// catalogLocationView — code+name, без flag/markup.
func catalogLocationView(l nodehost.Location) catalogLocationJSON {
	return catalogLocationJSON{Code: l.Code, Name: l.Name}
}

func catalogLimitsView(l *nodehost.Limits) catalogLimitsJSON {
	out := catalogLimitsJSON{Types: map[string]bool{}}
	if l == nil {
		return out
	}
	out.Cores = catalogRangeJSON{Min: l.Limits.CPU.Min, Max: l.Limits.CPU.Max}
	out.RamGB = catalogRangeJSON{Min: l.Limits.RAM.Min, Max: l.Limits.RAM.Max}
	out.DiskGB = catalogRangeJSON{Min: l.Limits.Disk.Min, Max: l.Limits.Disk.Max}
	if l.Types != nil {
		out.Types = l.Types
	}
	return out
}

// hostingErr — маппинг ошибок каталога/оценки: таймаут → 504, остальное → 502.
func hostingCatalogErr(w http.ResponseWriter, err error) {
	if hostingTimeoutErr(err) {
		controlAPIError(w, http.StatusGatewayTimeout, "provider_timeout", "provider did not answer in time")
		return
	}
	log.Printf("hosting catalog provider error: %v", err)
	controlAPIError(w, http.StatusBadGateway, "provider_error", "provider request failed")
}

// catalogerFor — провайдер каталога: false = 501 записан (beget/неизвестный).
func (r *Registry) catalogerFor(w http.ResponseWriter, c control.HostingConnection) (hostingCataloger, bool) {
	prov, err := r.hostingProviderFor(c)
	if err != nil {
		log.Printf("hosting catalog %d: provider unavailable: %v", c.ID, err)
		controlAPIError(w, http.StatusUnprocessableEntity, "provider_auth_failed", "provider rejected the credentials")
		return nil, false
	}
	cp, ok := prov.(hostingCataloger)
	if !ok {
		controlAPIError(w, http.StatusNotImplemented, "not_implemented", "catalog is not implemented for this provider")
		return nil, false
	}
	return cp, true
}

// GET /api/me/hosting-connections/{id}/catalog?location={code}
func (r *Registry) handleHostingCatalog(w http.ResponseWriter, req *http.Request) {
	c, ok := r.hostingOwned(w, req)
	if !ok {
		return
	}
	cp, ok := r.catalogerFor(w, c)
	if !ok {
		return
	}
	location := req.URL.Query().Get("location")
	if utf8.RuneCountInString(location) > 64 {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "invalid location")
		return
	}
	ctx, cancel := providerCtx(req)
	defer cancel()
	data, err := cp.Catalog(ctx, location)
	if err != nil {
		hostingCatalogErr(w, err)
		return
	}
	locations := make([]catalogLocationJSON, 0, len(data.Locations))
	for _, l := range data.Locations {
		locations = append(locations, catalogLocationView(l))
	}
	osList := make([]catalogOptionJSON, 0, len(data.OS))
	for _, t := range data.OS {
		// id = имя (именно оно идёт в CreateRequest.os).
		osList = append(osList, catalogOptionJSON{ID: t.Name, Name: t.Name})
	}
	disks := make([]catalogOptionJSON, 0, len(data.Disks))
	for _, d := range data.Disks {
		disks = append(disks, catalogOptionJSON{ID: d.Name, Name: d.Name})
	}
	if data.CPU == nil {
		data.CPU = []string{}
	}
	out := hostingCatalogJSON{
		Provider:  c.Provider,
		CheckedAt: time.Now().Unix(),
		Locations: locations,
		OS:        osList,
		CPUModels: data.CPU,
		DiskTypes: disks,
		Limits:    catalogLimitsView(data.Limits),
	}
	if data.Network != nil {
		out.Network = &catalogNetworkJSON{Speeds: data.Network.Speeds, Available: data.Network.HasAvailableNodes}
	}
	writeCabinetJSON(w, http.StatusOK, out)
}

// POST /api/me/cloud-resources/estimate — цена конфигурации без side-effect;
// безопасное чтение (как GET), поэтому без CSRF.
func (r *Registry) handleCloudEstimate(w http.ResponseWriter, req *http.Request) {
	var body struct {
		ConnectionID  int64 `json:"connection_id"`
		Configuration struct {
			Cores        int    `json:"cores"`
			RAM          int    `json:"ram"`
			Disk         int    `json:"disk"`
			NetworkSpeed string `json:"networkSpeed"`
			Period       string `json:"period"`
			Location     string `json:"location"`
		} `json:"configuration"`
	}
	if err := decodeCabinetJSON(w, req, &body); err != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "bad json")
		return
	}
	cfg := body.Configuration
	periods := map[string]bool{"1d": true, "3d": true, "1w": true, "1m": true, "3m": true, "6m": true, "1y": true}
	if body.ConnectionID <= 0 || cfg.Cores <= 0 || cfg.RAM <= 0 || cfg.Disk <= 0 ||
		(cfg.Period != "" && !periods[cfg.Period]) {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "cores, ram, disk must be positive; period is invalid")
		return
	}
	c, err := r.control.HostingConnectionOwned(body.ConnectionID, sessionFrom(req).user.ID)
	if err != nil {
		hostingControlErr(w, err)
		return
	}
	cp, ok := r.catalogerFor(w, c)
	if !ok {
		return
	}
	ctx, cancel := providerCtx(req)
	defer cancel()
	est, err := cp.Estimate(ctx, nodehost.EstimateRequest{
		Cores: cfg.Cores, RAM: cfg.RAM, Disk: cfg.Disk,
		NetworkSpeed: cfg.NetworkSpeed, Period: cfg.Period, Location: cfg.Location,
	})
	if err != nil {
		hostingCatalogErr(w, err)
		return
	}
	amount := est.PeriodPrice
	if amount == "" {
		amount = est.MonthlyPrice
	}
	writeCabinetJSON(w, http.StatusOK, map[string]any{
		"amount":      string(amount),
		"currency":    est.Currency,
		"period":      cfg.Period,
		"period_days": est.PeriodDays,
		"checked_at":  time.Now().Unix(),
		"scope":       "nodehost_api_pricing",
	})
}
