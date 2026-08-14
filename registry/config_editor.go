package main

import (
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// Редактирование конфигурации регистратора через панель.
//
// Hot-apply секции: shared_proxy (SNI, users), cloudflare (токен/зона/домены/ttl/proxied),
// node_defaults (интервалы нод), globalping (api_base/token), panel (token, events_max),
// rotation (лимит мастерства), healthcheck.quarantine_attempts (V7.9.12 — число
// GP-попыток в карантине до бана; читается на каждом отчёте, ticker'ов нет).
// Применённые значения сразу обслуживаются рантаймом (GET /config для нод, pushAssignments,
// верификация Globalping, авторизация панели), а затем персистятся обратно в TOML-файл
// (атомарно: temp + rename, с .bak-копией). Остальные healthcheck/http/state —
// намеренно read-only: их тайминги вшиты в запущенные ticker'ы, смена требует рестарта.
//
// Ограничение: удаление пользователя из shared_proxy.users НЕ удаляет его на нодах
// (ноды только добавляют пользователей в telemt через POST /v1/users).

type editableSharedProxy struct {
	TLSDomain string            `json:"tls_domain"`
	Users     map[string]string `json:"users"`
}

type editableCloudflare struct {
	APIToken string   `json:"api_token"`
	ZoneID   string   `json:"zone_id"`
	Domains  []string `json:"domains"`
	DNSTTL   int      `json:"dns_ttl"`
	Proxied  bool     `json:"proxied"`
}

type editableNodeDefaults struct {
	HeartbeatMs  int `json:"heartbeat_ms"`
	GlobalpingMs int `json:"globalping_ms"`
	MetricsMs    int `json:"metrics_ms"`
	SyncMs       int `json:"sync_ms"`
}

type editableGlobalping struct {
	APIBase string `json:"api_base"`
}

type editablePanel struct {
	Token     string `json:"token"`
	EventsMax int    `json:"events_max"`
}

// editableRotation — V7.9.3: лимит времени мастерства (мин; 0 = выкл.).
type editableRotation struct {
	MasterTTLMinutes int `json:"master_ttl_minutes"`
}

// editableHealthcheck — V7.9.12: число GP-попыток карантина до бана по IP.
type editableHealthcheck struct {
	QuarantineAttempts int `json:"quarantine_attempts"`
}

// editableConfig — тело PUT /panel/api/config; nil-секция = не трогать (merge-семантика).
// Map-пользователи заменяются ЦЕЛИКОМ (клиент шлёт желаемое конечное состояние).
type editableConfig struct {
	SharedProxy  *editableSharedProxy  `json:"shared_proxy,omitempty"`
	Cloudflare   *editableCloudflare   `json:"cloudflare,omitempty"`
	NodeDefaults *editableNodeDefaults `json:"node_defaults,omitempty"`
	Globalping   *editableGlobalping   `json:"globalping,omitempty"`
	Panel        *editablePanel        `json:"panel,omitempty"`
	Rotation     *editableRotation     `json:"rotation,omitempty"`
	Healthcheck  *editableHealthcheck  `json:"healthcheck,omitempty"`
}

var (
	hostnameRe = regexp.MustCompile(`^(?i)[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)
	usernameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	secretRe   = regexp.MustCompile(`(?i)^[0-9a-f]{32}$`)
)

func validHostname(s string) bool {
	return len(s) <= 253 && hostnameRe.MatchString(s)
}

// validateEditable — полная валидация присланных секций; nil = ок.
func validateEditable(in *editableConfig) error {
	if sp := in.SharedProxy; sp != nil {
		if !validHostname(sp.TLSDomain) {
			return fmt.Errorf("shared_proxy.tls_domain: %q не hostname", sp.TLSDomain)
		}
		if len(sp.Users) == 0 {
			return fmt.Errorf("shared_proxy.users: хотя бы один пользователь обязателен")
		}
		if len(sp.Users) > 64 {
			return fmt.Errorf("shared_proxy.users: не больше 64 пользователей")
		}
		for name, secret := range sp.Users {
			if !usernameRe.MatchString(name) {
				return fmt.Errorf("shared_proxy.users: недопустимое имя %q", name)
			}
			if !secretRe.MatchString(secret) {
				return fmt.Errorf("shared_proxy.users[%s]: secret должен быть 32 hex-символа", name)
			}
		}
	}
	if cf := in.Cloudflare; cf != nil {
		if strings.TrimSpace(cf.APIToken) == "" || len(cf.APIToken) > 200 {
			return fmt.Errorf("cloudflare.api_token: пусто или длиннее 200 символов")
		}
		if strings.TrimSpace(cf.ZoneID) == "" || len(cf.ZoneID) > 64 {
			return fmt.Errorf("cloudflare.zone_id: пусто или длиннее 64 символов")
		}
		if len(cf.Domains) == 0 || len(cf.Domains) > 20 {
			return fmt.Errorf("cloudflare.domains: нужно 1..20 доменов")
		}
		for _, d := range cf.Domains {
			if !validHostname(strings.TrimSpace(d)) {
				return fmt.Errorf("cloudflare.domains: %q не hostname", d)
			}
		}
		if cf.DNSTTL != 1 && (cf.DNSTTL < 60 || cf.DNSTTL > 86400) {
			return fmt.Errorf("cloudflare.dns_ttl: 1 (auto) или 60..86400")
		}
	}
	if nd := in.NodeDefaults; nd != nil {
		bounds := []struct {
			name      string
			v, lo, hi int
		}{
			{"heartbeat_ms", nd.HeartbeatMs, 3000, 120000},
			{"globalping_ms", nd.GlobalpingMs, 60000, 7200000},
			{"metrics_ms", nd.MetricsMs, 5000, 600000},
			{"sync_ms", nd.SyncMs, 5000, 600000},
		}
		for _, b := range bounds {
			if b.v < b.lo || b.v > b.hi {
				return fmt.Errorf("node_defaults.%s: допустимо %d..%d, получено %d", b.name, b.lo, b.hi, b.v)
			}
		}
	}
	if gp := in.Globalping; gp != nil {
		if gp.APIBase != "" && !strings.HasPrefix(gp.APIBase, "https://") && !strings.HasPrefix(gp.APIBase, "http://") {
			return fmt.Errorf("globalping.api_base: ожидается http(s):// URL")
		}
	}
	if p := in.Panel; p != nil {
		if len(p.Token) > 128 {
			return fmt.Errorf("panel.token: длиннее 128 символов")
		}
		if p.EventsMax < 50 || p.EventsMax > 10000 {
			return fmt.Errorf("panel.events_max: допустимо 50..10000")
		}
	}
	if rot := in.Rotation; rot != nil {
		if rot.MasterTTLMinutes < 0 || rot.MasterTTLMinutes > maxMasterTTLMinutes {
			return fmt.Errorf("rotation.master_ttl_minutes: допустимо 0..%d мин (0 = без лимита), получено %d",
				maxMasterTTLMinutes, rot.MasterTTLMinutes)
		}
	}
	if hc := in.Healthcheck; hc != nil {
		if hc.QuarantineAttempts < 1 || hc.QuarantineAttempts > 20 {
			return fmt.Errorf("healthcheck.quarantine_attempts: допустимо 1..20, получено %d", hc.QuarantineAttempts)
		}
	}
	return nil
}

// staticConfigInfo — секции, требующие рестарта (для read-only отображения в панели).
type staticConfigInfo struct {
	HTTPAddr            string `json:"http_addr"`
	StateFile           string `json:"state_file"`
	ProbeIntervalMs     int    `json:"probe_interval_ms"`
	ProbeTimeoutMs      int    `json:"probe_timeout_ms"`
	SelectionIntervalMs int    `json:"selection_interval_ms"`
	HeartbeatTTLSec     int    `json:"heartbeat_ttl_sec"`
	FailThreshold       int    `json:"fail_threshold"`
	RecoverThreshold    int    `json:"recover_threshold"`
	ReportFreshnessMin  int    `json:"report_freshness_min"`
	PruneUnhealthyMin   int    `json:"prune_unhealthy_min"` // эффективное (дефолт 60)
}

// handleGetConfig — GET /panel/api/config: полная эффективная конфигурация.
// Секреты отдаются в открытом виде: endpoint закрыт panel-токеном (admin),
// а редактирование без показа текущих значений неюзабельно.
func (r *Registry) handleGetConfig(w http.ResponseWriter, req *http.Request) {
	if !r.panelAuthorized(req) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	r.cfgMu.RLock()
	cfg := editableConfig{
		SharedProxy: &editableSharedProxy{
			TLSDomain: r.cfg.SharedProxy.TLSDomain,
			Users:     mapsClone(r.cfg.SharedProxy.Users),
		},
		Cloudflare: &editableCloudflare{
			APIToken: r.cfg.Cloudflare.APIToken,
			ZoneID:   r.cfg.Cloudflare.ZoneID,
			Domains:  append([]string(nil), r.cfg.Cloudflare.Domains...),
			DNSTTL:   r.cfg.Cloudflare.DNSTTL,
			Proxied:  r.cfg.Cloudflare.Proxied,
		},
		NodeDefaults: &editableNodeDefaults{
			HeartbeatMs:  r.cfg.NodeDefaults.HeartbeatMs,
			GlobalpingMs: r.cfg.NodeDefaults.GlobalpingMs,
			MetricsMs:    r.cfg.NodeDefaults.MetricsMs,
			SyncMs:       r.cfg.NodeDefaults.SyncMs,
		},
		Globalping: &editableGlobalping{
			APIBase: r.cfg.Globalping.APIBase,
		},
		Panel: &editablePanel{
			Token:     r.cfg.Panel.Token,
			EventsMax: r.cfg.Panel.EventsMax,
		},
		// Эффективное значение (ключа нет в файле → дефолт 30) — панель
		// показывает тот лимит, который реально действует.
		Rotation: &editableRotation{
			MasterTTLMinutes: resolveMasterTTLMinutes(r.cfg.Rotation.MasterTTLMinutes),
		},
		// Эффективное значение (max(1, …) при загрузке) — панель видит то,
		// что реально действует.
		Healthcheck: &editableHealthcheck{
			QuarantineAttempts: r.cfg.QuarantineAttempts,
		},
	}
	static := staticConfigInfo{
		HTTPAddr:            r.cfg.HTTP.Addr,
		StateFile:           r.cfg.State.File,
		ProbeIntervalMs:     r.cfg.Healthcheck.ProbeIntervalMs,
		ProbeTimeoutMs:      r.cfg.Healthcheck.ProbeTimeoutMs,
		SelectionIntervalMs: r.cfg.Healthcheck.SelectionIntervalMs,
		HeartbeatTTLSec:     r.cfg.Healthcheck.HeartbeatTTLSec,
		FailThreshold:       r.cfg.Healthcheck.FailThreshold,
		RecoverThreshold:    r.cfg.Healthcheck.RecoverThreshold,
		ReportFreshnessMin:  r.cfg.Healthcheck.ReportFreshnessMin,
		PruneUnhealthyMin:   resolvePruneUnhealthyMinutes(r.cfg.Healthcheck.PruneUnhealthyMin),
	}
	r.cfgMu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"config": cfg, "static": static})
}

func mapsClone(m map[string]string) map[string]string {
	return maps.Clone(m)
}

// handlePutConfig — PUT /panel/api/config: валидация → hot-apply → персист → аудит.
// При смене cloudflare-секции и наличии мастера — немедленный DNS-push.
func (r *Registry) handlePutConfig(w http.ResponseWriter, req *http.Request) {
	if !r.panelAuthorized(req) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var in editableConfig
	dec := json.NewDecoder(req.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateEditable(&in); err != nil {
		http.Error(w, "validation: "+err.Error(), http.StatusBadRequest)
		return
	}

	// собрать нового CF-клиента ЗАРАНЕЕ (вне локов), если токен меняется
	var newCF cfDNSAPI
	if in.Cloudflare != nil {
		cf, err := newCFClient(in.Cloudflare.APIToken)
		if err != nil {
			http.Error(w, "cloudflare client: "+err.Error(), http.StatusBadRequest)
			return
		}
		newCF = cf
	}

	changed := make([]string, 0, 5)
	var persistErr error

	r.cfgMu.Lock()
	if sp := in.SharedProxy; sp != nil {
		r.cfg.SharedProxy.TLSDomain = strings.TrimSpace(sp.TLSDomain)
		r.cfg.SharedProxy.Users = mapsClone(sp.Users)
		changed = append(changed, "shared_proxy(sni,users)")
	}
	if cf := in.Cloudflare; cf != nil {
		r.cfg.Cloudflare.APIToken = strings.TrimSpace(cf.APIToken)
		r.cfg.Cloudflare.ZoneID = strings.TrimSpace(cf.ZoneID)
		domains := make([]string, 0, len(cf.Domains))
		for _, d := range cf.Domains {
			domains = append(domains, strings.TrimSpace(d))
		}
		r.cfg.Cloudflare.Domains = domains
		r.cfg.Cloudflare.DNSTTL = cf.DNSTTL
		r.cfg.Cloudflare.Proxied = cf.Proxied
		r.cf = newCF
		changed = append(changed, "cloudflare(token,zone,domains,ttl,proxied)")
	}
	if nd := in.NodeDefaults; nd != nil {
		r.cfg.NodeDefaults.HeartbeatMs = nd.HeartbeatMs
		r.cfg.NodeDefaults.GlobalpingMs = nd.GlobalpingMs
		r.cfg.NodeDefaults.MetricsMs = nd.MetricsMs
		r.cfg.NodeDefaults.SyncMs = nd.SyncMs
		changed = append(changed, "node_defaults")
	}
	if gp := in.Globalping; gp != nil {
		if gp.APIBase != "" {
			r.cfg.Globalping.APIBase = strings.TrimRight(gp.APIBase, "/")
		}
		changed = append(changed, "globalping")
	}
	if p := in.Panel; p != nil {
		r.cfg.Panel.Token = p.Token
		r.cfg.Panel.EventsMax = p.EventsMax
		changed = append(changed, "panel(token,events_max)")
	}
	if rot := in.Rotation; rot != nil {
		v := rot.MasterTTLMinutes
		r.cfg.Rotation.MasterTTLMinutes = &v
		changed = append(changed, fmt.Sprintf("rotation(master_ttl=%dmin)", v))
	}
	if hc := in.Healthcheck; hc != nil {
		// hot-apply: читается в handleHealthReport на каждом отчёте;
		// в RegistryConfig — для персиста в TOML.
		r.cfg.QuarantineAttempts = hc.QuarantineAttempts
		r.cfg.Healthcheck.QuarantineAttempts = hc.QuarantineAttempts
		changed = append(changed, fmt.Sprintf("healthcheck(quarantine_attempts=%d)", hc.QuarantineAttempts))
	}
	if len(changed) > 0 {
		persistErr = r.persistConfigLocked()
	}
	r.cfgMu.Unlock()

	if len(changed) == 0 {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "changed": []string{}})
		return
	}

	// аудит в журнал событий
	r.mu.Lock()
	r.addEventLocked(Event{
		Type:   EventConfigChanged,
		Detail: "via panel: " + strings.Join(changed, ", "),
	})
	r.persistStateLocked()
	r.mu.Unlock()
	log.Printf("config changed via panel: %s (persisted: %v)", strings.Join(changed, ", "), persistErr == nil)

	// cloudflare-секция менялась → сначала раскладка (новым доменам нужен мастер),
	// потом немедленная запись всех записей по назначениям, и в конце —
	// зачистка доменов, выведенных из managed-списка (V7.8; только тех, кем
	// управляли раньше — чужие записи зоны не трогаем)
	push := map[string]any{}
	if in.Cloudflare != nil {
		r.evaluateAssignments(time.Now())
		updated, errs := r.pushAssignments()
		push["dns_push"] = map[string]any{"updated": updated, "errors": errs}
		r.sweepOrphans()
	}

	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{
		"ok":        true,
		"changed":   changed,
		"persisted": persistErr == nil,
	}
	if persistErr != nil {
		resp["persist_warning"] = persistErr.Error()
	}
	for k, v := range push {
		resp[k] = v
	}
	json.NewEncoder(w).Encode(resp)
}

// pushAssignments — немедленная запись A-записей всех managed-доменов, КАЖДОГО
// на IP своего назначенного мастера (панель: после правок cloudflare-секции
// или кнопка «Перезаписать DNS»).
func (r *Registry) pushAssignments() (updated []string, errs []string) {
	r.cfgMu.RLock()
	rawDomains := append([]string(nil), r.cfg.Cloudflare.Domains...)
	r.cfgMu.RUnlock()

	type target struct{ nodeID, ip string }
	names := make([]string, 0, len(rawDomains))
	targets := make([]target, 0, len(rawDomains))
	r.mu.RLock()
	for _, d := range rawDomains {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		var t target
		if c := r.state.Candidates[r.state.Assignments[d]]; c != nil {
			t = target{c.NodeID, c.IP}
		}
		names = append(names, d)
		targets = append(targets, t)
	}
	r.mu.RUnlock()

	for i, d := range names {
		t := targets[i]
		if t.ip == "" {
			errs = append(errs, d+": no assigned master")
			continue
		}
		if err := r.upsertARecord(d, t.ip); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", d, err))
		} else {
			updated = append(updated, fmt.Sprintf("%s → %s", d, t.ip))
		}
	}

	r.mu.Lock()
	for range updated {
		r.state.Counters.DNSUpdates++
	}
	if len(updated) > 0 {
		r.addEventLocked(Event{Type: EventDNSUpdated, Detail: strings.Join(updated, ", ") + " (manual push)"})
	}
	r.state.Counters.DNSErrors += len(errs)
	if len(errs) > 0 {
		r.addEventLocked(Event{Type: EventDNSError, Detail: strings.Join(errs, "; ") + " (manual push)"})
	}
	r.persistStateLocked()
	r.mu.Unlock()
	return updated, errs
}

// handleDNSPush — POST /panel/api/dns-push.
func (r *Registry) handleDNSPush(w http.ResponseWriter, req *http.Request) {
	if !r.panelAuthorized(req) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	updated, errs := r.pushAssignments()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"updated": updated, "errors": errs})
}

// persistConfigLocked перезаписывает TOML-файл конфигурации текущим состоянием.
// Формат: полный Marshal (комментарии файла теряются — оригинал остаётся в .bak).
// Атомарно: temp в том же каталоге + rename; mode сохраняется от старого файла.
// ВЫЗЫВАТЬ под cfgMu (write).
func (r *Registry) persistConfigLocked() error {
	path := r.cfg.configPath
	data, err := toml.Marshal(r.cfg.RegistryConfig)
	if err != nil {
		return fmt.Errorf("marshal toml: %w", err)
	}

	mode := os.FileMode(0660)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
		if bak, rerr := os.ReadFile(path); rerr == nil {
			if werr := os.WriteFile(path+".bak", bak, 0600); werr != nil {
				log.Printf("config backup write error: %v", werr)
			}
		}
	}

	tmp := filepath.Join(filepath.Dir(path), fmt.Sprintf(".%s.tmp-%d", filepath.Base(path), time.Now().UnixNano()))
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return fmt.Errorf("write tmp config: %w", err)
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("chmod tmp config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename config: %w", err)
	}
	return nil
}
