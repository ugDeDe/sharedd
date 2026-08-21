package main

import (
	"bytes"
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
// rotation (лимит мастерства), healthcheck.quarantine_attempts (— число
// GP-попыток в карантине до бана; читается на каждом отчёте, ticker'ов нет).
// Применённые значения сразу обслуживаются рантаймом (GET /config для нод, pushAssignments,
// верификация Globalping, авторизация панели), а затем персистятся обратно в TOML-файл
// (атомарно: temp + rename, с.bak-копией). Остальные healthcheck/http/state —
// намеренно read-only: их тайминги вшиты в запущенные ticker'ы, смена требует рестарта.
//
// Ограничение: удаление пользователя из shared_proxy.users НЕ удаляет его на нодах.
// Добавление и ротация secret синхронизируются через конфиг telemt.

type editableSharedProxy struct {
	TLSDomain string            `json:"tls_domain"`
	Port      int               `json:"port"`
	Users     map[string]string `json:"users"`
	hasTLS    bool
	hasPort   bool
	hasUsers  bool
}

func (s *editableSharedProxy) UnmarshalJSON(data []byte) error {
	type sharedProxyJSON struct {
		TLSDomain *string            `json:"tls_domain"`
		Port      *int               `json:"port"`
		Users     *map[string]string `json:"users"`
	}
	var in sharedProxyJSON
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return err
	}
	if in.TLSDomain != nil {
		s.TLSDomain, s.hasTLS = *in.TLSDomain, true
	}
	if in.Port != nil {
		s.Port, s.hasPort = *in.Port, true
	}
	if in.Users != nil {
		s.Users, s.hasUsers = *in.Users, true
	}
	return nil
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

// EditableRotation — лимит времени мастерства (мин; 0 = выкл.).
type editableRotation struct {
	MasterTTLMinutes int `json:"master_ttl_minutes"`
}

// EditableHealthcheck — число GP-попыток карантина до бана по IP.
type editableHealthcheck struct {
	QuarantineAttempts    int  `json:"quarantine_attempts"`
	GlobalpingValidityMin *int `json:"globalping_validity_min,omitempty"`
}

// EditableSRMD — Система Распределения и Масштабирования Доменов.
// Enabled=false по умолчанию — автоматическое создание доменов должно быть
// явным выбором оператора. BaseDomain пусто = первый из cloudflare.domains.
type editableSRMD struct {
	Enabled           bool   `json:"enabled"`
	BaseDomain        string `json:"base_domain"`
	MaxNodesPerDomain int    `json:"max_nodes_per_domain"`
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
	SRMD         *editableSRMD         `json:"srmd,omitempty"`
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
		if !sp.hasTLS && !sp.hasPort && !sp.hasUsers {
			return fmt.Errorf("shared_proxy: укажите tls_domain, port или users")
		}
		if sp.hasTLS && !validHostname(sp.TLSDomain) {
			return fmt.Errorf("shared_proxy.tls_domain: %q не hostname", sp.TLSDomain)
		}
		if sp.hasPort && (sp.Port < 1 || sp.Port > 65535) {
			return fmt.Errorf("shared_proxy.port: допустимо 1..65535")
		}
		if sp.hasUsers && len(sp.Users) == 0 {
			return fmt.Errorf("shared_proxy.users: хотя бы один пользователь обязателен")
		}
		if sp.hasUsers && len(sp.Users) > 64 {
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
		if len(cf.Domains) == 0 || len(cf.Domains) > 50 {
			return fmt.Errorf("cloudflare.domains: нужно 1..50 доменов")
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
		if len(strings.TrimSpace(p.Token)) < 16 || len(p.Token) > 128 {
			return fmt.Errorf("panel.token: требуется 16..128 символов")
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
		if hc.GlobalpingValidityMin != nil && (*hc.GlobalpingValidityMin < 1 || *hc.GlobalpingValidityMin > 525600) {
			return fmt.Errorf("healthcheck.globalping_validity_min: допустимо 1..525600, получено %d", *hc.GlobalpingValidityMin)
		}
	}
	if s := in.SRMD; s != nil {
		if b := strings.TrimSpace(s.BaseDomain); b != "" && !validHostname(b) {
			return fmt.Errorf("srmd.base_domain: %q не hostname", b)
		}
		if s.MaxNodesPerDomain < 1 || s.MaxNodesPerDomain > 1000 {
			return fmt.Errorf("srmd.max_nodes_per_domain: допустимо 1..1000, получено %d", s.MaxNodesPerDomain)
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

const nodeInstallerURL = "https://raw.githubusercontent.com/ugDeDe/sharedd/main/scripts/install_node_web.sh"

type nodeOnboardingInfo struct {
	Token          string `json:"token"`
	RegistryURL    string `json:"registry_url,omitempty"`
	InstallCommand string `json:"install_command,omitempty"`
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func nodeInstallCommand(registryURL, token string) string {
	if registryURL == "" {
		return ""
	}
	return "curl -fsSL " + shellQuote(nodeInstallerURL) + " | sudo bash -s -- --registry=" + shellQuote(registryURL) + " --registry-token=" + shellQuote(token)
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
	proxyPort := r.cfg.SharedProxy.Port
	if proxyPort == 0 {
		proxyPort = 443
	}
	cfg := editableConfig{
		SharedProxy: &editableSharedProxy{
			TLSDomain: r.cfg.SharedProxy.TLSDomain,
			Port:      proxyPort,
			Users:     mapsClone(r.cfg.SharedProxy.Users),
			hasTLS:    true,
			hasUsers:  true,
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
			QuarantineAttempts:    r.cfg.QuarantineAttempts,
			GlobalpingValidityMin: intPtr(r.cfg.Healthcheck.GlobalpingValidityMin),
		},
		// СРМД — эффективные значения (enabled по умолчанию выкл).
		SRMD: &editableSRMD{
			Enabled:           r.cfg.SRMD.Enabled != nil && *r.cfg.SRMD.Enabled,
			BaseDomain:        r.cfg.SRMD.BaseDomain,
			MaxNodesPerDomain: resolveSRMDMaxNodes(r.cfg.SRMD.MaxNodesPerDomain),
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
	onboarding := nodeOnboardingInfo{
		Token:       r.cfg.Security.NodeToken,
		RegistryURL: r.cfg.Panel.PublicURL,
	}
	onboarding.InstallCommand = nodeInstallCommand(onboarding.RegistryURL, onboarding.Token)
	r.cfgMu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"config": cfg, "static": static, "node_onboarding": onboarding})
}

func mapsClone(m map[string]string) map[string]string {
	return maps.Clone(m)
}

func intPtr(v int) *int { return &v }

// orDash — пустое значение в журналах/деталях показываем прочерком.
func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
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
	r.cfgMu.RLock()
	globalpingMs := r.cfg.NodeDefaults.GlobalpingMs
	validityMin := r.cfg.Healthcheck.GlobalpingValidityMin
	r.cfgMu.RUnlock()
	if globalpingMs == 0 {
		globalpingMs = 300000
	}
	if validityMin == 0 {
		validityMin = 15
	}
	if in.NodeDefaults != nil {
		globalpingMs = in.NodeDefaults.GlobalpingMs
	}
	if in.Healthcheck != nil && in.Healthcheck.GlobalpingValidityMin != nil {
		validityMin = *in.Healthcheck.GlobalpingValidityMin
	}
	if time.Duration(validityMin)*time.Minute <= time.Duration(globalpingMs)*time.Millisecond {
		http.Error(w, "validation: healthcheck.globalping_validity_min должен быть больше node_defaults.globalping_ms", http.StatusBadRequest)
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
	sharedPortChanged := false
	newSharedPort := 0

	r.cfgMu.Lock()
	if sp := in.SharedProxy; sp != nil {
		parts := make([]string, 0, 3)
		if sp.hasTLS {
			r.cfg.SharedProxy.TLSDomain = strings.TrimSpace(sp.TLSDomain)
			parts = append(parts, "sni")
		}
		if sp.hasPort {
			r.cfg.SharedProxy.Port = sp.Port
			sharedPortChanged, newSharedPort = true, sp.Port
			parts = append(parts, "port")
		}
		if sp.hasUsers {
			r.cfg.SharedProxy.Users = mapsClone(sp.Users)
			parts = append(parts, "users")
		}
		changed = append(changed, "shared_proxy("+strings.Join(parts, ",")+")")
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
		if hc.GlobalpingValidityMin != nil {
			r.cfg.Healthcheck.GlobalpingValidityMin = *hc.GlobalpingValidityMin
			r.cfg.GlobalpingValidityTTL = time.Duration(*hc.GlobalpingValidityMin) * time.Minute
		} else if r.cfg.Healthcheck.GlobalpingValidityMin == 0 {
			r.cfg.Healthcheck.GlobalpingValidityMin = validityMin
			r.cfg.GlobalpingValidityTTL = time.Duration(validityMin) * time.Minute
		}
		changed = append(changed, fmt.Sprintf("healthcheck(quarantine_attempts=%d,gp_validity=%dmin)", hc.QuarantineAttempts, r.cfg.Healthcheck.GlobalpingValidityMin))
	}
	if s := in.SRMD; s != nil {
		// Hot-apply — читается srmdRebalanceLocked на каждом тике
		// селекции; эффект (создание/сворачивание доменов) наступает после
		// srmdStableTicks устойчивого условия.
		v := s.Enabled
		r.cfg.SRMD.Enabled = &v
		r.cfg.SRMD.BaseDomain = strings.TrimSpace(s.BaseDomain)
		r.cfg.SRMD.MaxNodesPerDomain = s.MaxNodesPerDomain
		changed = append(changed, fmt.Sprintf("srmd(enabled=%t,base=%s,max_nodes_per_domain=%d)",
			s.Enabled, orDash(r.cfg.SRMD.BaseDomain), s.MaxNodesPerDomain))
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
	if sharedPortChanged {
		for _, c := range r.state.Candidates {
			compatible := c.Port == newSharedPort
			c.PortCompatible = &compatible
		}
	}
	r.addEventLocked(Event{
		Type:   EventConfigChanged,
		Detail: "via panel: " + strings.Join(changed, ", "),
	})
	r.persistStateLocked()
	r.mu.Unlock()
	log.Printf("config changed via panel: %s (persisted: %v)", strings.Join(changed, ", "), persistErr == nil)
	if sharedPortChanged {
		r.evaluateAssignments(time.Now())
	}

	// cloudflare-секция менялась → сначала раскладка (новым доменам нужен мастер),
	// потом немедленная запись всех записей по назначениям, и в конце —
	// зачистка доменов, выведенных из managed-списка (; только тех, кем
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

	type target struct{ nodeID, ip, cname string }
	names := make([]string, 0, len(rawDomains))
	targets := make([]target, 0, len(rawDomains))
	r.mu.RLock()
	for _, d := range rawDomains {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		var t target
		// Свёрнутые СРМД домены пушим CNAME'ом, а не A-записью
		if cn := r.state.SRMD.CNames[d]; cn != "" {
			t.cname = cn
		} else if c := r.state.Candidates[r.state.Assignments[d]]; c != nil {
			t = target{nodeID: c.NodeID, ip: c.IP}
		}
		names = append(names, d)
		targets = append(targets, t)
	}
	r.mu.RUnlock()

	for i, d := range names {
		t := targets[i]
		if t.cname != "" {
			r.mu.Lock()
			r.enqueueDNSDesiredLocked(d, "CNAME", t.cname, "")
			r.persistStateLocked()
			r.mu.Unlock()
			if err := r.reconcileDNSDomain(d, true); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", d, err))
			} else {
				updated = append(updated, fmt.Sprintf("%s → CNAME %s", d, t.cname))
			}
			continue
		}
		if t.ip == "" {
			errs = append(errs, d+": no assigned master")
			continue
		}
		r.mu.Lock()
		r.enqueueDNSDesiredLocked(d, "A", t.ip, t.nodeID)
		r.persistStateLocked()
		r.mu.Unlock()
		if err := r.reconcileDNSDomain(d, true); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", d, err))
		} else {
			updated = append(updated, fmt.Sprintf("%s → %s", d, t.ip))
		}
	}

	r.mu.Lock()
	if len(updated) > 0 {
		r.addEventLocked(Event{Type: EventDNSUpdated, Detail: strings.Join(updated, ", ") + " (manual push requested)"})
	}
	if len(errs) > 0 {
		r.addEventLocked(Event{Type: EventDNSError, Detail: strings.Join(errs, "; ") + " (manual push requested)"})
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
// Формат: полный Marshal (комментарии файла теряются — оригинал остаётся в.bak).
// Атомарно: temp в том же каталоге + rename; mode сохраняется от старого файла.
// ВЫЗЫВАТЬ под cfgMu (write).
func (r *Registry) persistConfigLocked() error {
	path := r.cfg.configPath
	if path == "" {
		return fmt.Errorf("config path is empty (registry запущен без -config?)")
	}
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
