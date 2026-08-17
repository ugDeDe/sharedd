package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// Терминальные классы и история (см. terminate.go, db.go).
const (
	defaultTerminateDeadMinutes = 10 // dead-класс: TCP+metrics молчат дольше — нода завершается
	defaultQuarantineAttempts   = 3  // неудачных верифицированных GP-проверок в карантине → бан по IP
	defaultEventsRetentionDays  = 30 // ротация журнала событий в БД (баны НЕ ротируются никогда)
)

// defaultDBFileFor — файл истории по умолчанию: sibling state-файла.
func defaultDBFileFor(stateFile string) string {
	if d := dirOf(stateFile); d != "" {
		return d + "/registry.db"
	}
	return "registry.db"
}

// dirOf — dirname без filepath (state-файлы у нас без экзотики).
func dirOf(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return ""
}

type RegistryConfig struct {
	HTTP struct {
		Addr string `toml:"addr"`
	} `toml:"http"`

	State struct {
		File string `toml:"file"`
	} `toml:"state"`

	Healthcheck struct {
		ProbeIntervalMs     int `toml:"probe_interval_ms"`
		ProbeTimeoutMs      int `toml:"probe_timeout_ms"`
		SelectionIntervalMs int `toml:"selection_interval_ms"`
		HeartbeatTTLSec     int `toml:"heartbeat_ttl_sec"`
		FailThreshold       int `toml:"fail_threshold"`
		RecoverThreshold    int `toml:"recover_threshold"`
		ReportFreshnessMin  int `toml:"report_freshness_min"`
		// PruneUnhealthyMin — через сколько минут НЕПРЕРЫВНО
		// нездоровая нода (вне очереди мастерства) удаляется из пула.
		// *int как у master_ttl_minutes: ключ отсутствует = дефолт 60,
		// явный 0 = рипер выключен.
		PruneUnhealthyMin *int `toml:"prune_unhealthy_min"`
		// TerminateDeadMin — терминальный класс «dead»: TCP-порт
		// не отвечает И метрики не поступают непрерывно дольше этого окна →
		// нода завершается навсегда (MsgDead в её лог). nil = дефолт 10,
		// явный 0 = выкл (класс разруливает только prune/expiry).
		TerminateDeadMin *int `toml:"terminate_dead_min"`
		// QuarantineAttempts — сколько ПОДРЯД неудачных
		// независимо верифицированных GP-проверок в карантине (включая ту,
		// что ноду туда посадила) переводят ноду в бан по IP (MsgIPBan).
		// 0/отсутствует = дефолт 3.
		QuarantineAttempts int `toml:"quarantine_attempts"`
	} `toml:"healthcheck"`

	// Database — вечная история в SQLite (events + терминальные
	// баны): питается /dashboard. File отсутствует = рядом со state-файлом
	// (registry.db); явно пустой File нельзя задать — отключение через
	// enabled=false. События ротируются (events_retention_days, дефолт 30),
	// баны — никогда (метрики дашборда «на постоянной основе»).
	Database struct {
		Enabled             *bool  `toml:"enabled"`
		File                string `toml:"file"`
		EventsRetentionDays int    `toml:"events_retention_days"`
	} `toml:"database"`

	// NodeDefaults — интервалы, раздаваемые нодам через GET /config.
	// Ноды применяют их динамически на каждом sync.
	NodeDefaults struct {
		HeartbeatMs  int `toml:"heartbeat_ms"`
		GlobalpingMs int `toml:"globalping_ms"`
		MetricsMs    int `toml:"metrics_ms"`
		SyncMs       int `toml:"sync_ms"`
	} `toml:"node_defaults"`

	Cloudflare struct {
		APIToken string   `toml:"api_token"`
		ZoneID   string   `toml:"zone_id"`
		Domains  []string `toml:"domains"`
		DNSTTL   int      `toml:"dns_ttl"`
		Proxied  bool     `toml:"proxied"`
	} `toml:"cloudflare"`

	// Rotation — принудительная ротация мастерства.
	// MasterTTLMinutes — максимум НЕПРЕРЫВНОГО времени ноды мастером одного
	// домена: по истечении selectionLoop насильно передаёт домен следующей
	// здоровой ноде (round-robin по очереди). Указатель: ключ ОТСУТСТВУЕТ →
	// дефолт defaultMasterTTLMinutes; явные 0 = без лимита; N = N минут.
	// Правится из панели на лету (config_editor.go).
	Rotation struct {
		MasterTTLMinutes *int `toml:"master_ttl_minutes"`
	} `toml:"rotation"`

	Globalping struct {
		APIBase string `toml:"api_base"`
	} `toml:"globalping"`

	// SRMD — Система Распределения и Масштабирования Доменов.
	// Следит за соотношением здоровой очереди нод и числа managed-доменов и
	// держит не больше MaxNodesPerDomain нод на домен: при росте пула
	// создаёт сиротские домены с инкрементом от BaseDomain (shared.example.com
	// → shared1.example.com, shared2.…), при shrinking'е — сворачивает лишние
	// в CNAME на оставшиеся, балансируя по активным клиентам. Подробности —
	// в srmd.go. Enabled НИКОГДА не включается по умолчанию (nil = false):
	// автоматическое создание доменов — явный выбор оператора.
	SRMD struct {
		Enabled           *bool  `toml:"enabled"`
		BaseDomain        string `toml:"base_domain"` // пусто = первый из cloudflare.domains
		MaxNodesPerDomain int    `toml:"max_nodes_per_domain"`
	} `toml:"srmd"`

	SharedProxy struct {
		TLSDomain string            `toml:"tls_domain"`
		Users     map[string]string `toml:"users"`
	} `toml:"shared_proxy"`

	// Panel — встроенная веб-панель мониторинга (ноды, доступность, журнал
	// событий). Enabled: nil (не задано) = включена. Token защищает API
	// панели и /status (Bearer); пустой токен = без авторизации (dev-режим,
	// предупреждение в логе). EventsMax — размер кольцевого буфера журнала.
	Panel struct {
		Enabled   *bool  `toml:"enabled"`
		Token     string `toml:"token"`
		EventsMax int    `toml:"events_max"`
	} `toml:"panel"`
}

type resolvedRegistryConfig struct {
	RegistryConfig
	ProbeInterval      time.Duration
	ProbeTimeout       time.Duration
	SelectionInterval  time.Duration
	HeartbeatTTL       time.Duration
	ReportFreshnessTTL time.Duration
	PruneUnhealthyTTL  time.Duration // 0 = рипер выключен
	PanelEnabled       bool
	// Терминальные классы и история.
	TerminateDeadTTL   time.Duration // 0 = dead-класс выключен
	QuarantineAttempts int           // ≥1
	EventsRetention    time.Duration
	DBEnabled          bool

	// configPath — путь к исходному TOML; нужен, чтобы правки из панели
	// персистить обратно в конфиг (см. config_editor.go).
	configPath string
}

func configPathFlag() string {
	fs := flag.NewFlagSet("registry", flag.ContinueOnError)
	path := fs.String("config", "", "path to registry TOML config file")
	_ = fs.Parse(os.Args[1:])
	if *path != "" {
		return *path
	}
	if v := os.Getenv("REGISTRY_CONFIG_PATH"); v != "" {
		return v
	}
	return "/etc/sharedd/registry.toml"
}

func loadRegistryConfig() (*resolvedRegistryConfig, error) {
	path := configPathFlag()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config %s: %w", path, err)
	}

	var cfg RegistryConfig
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config %s: %w", path, err)
	}

	applyRegistryDefaults(&cfg)

	if cfg.Cloudflare.APIToken == "" || cfg.Cloudflare.ZoneID == "" || len(cfg.Cloudflare.Domains) == 0 {
		return nil, fmt.Errorf("cloudflare.api_token, cloudflare.zone_id and cloudflare.domains are required in %s", path)
	}
	if cfg.SharedProxy.TLSDomain == "" || len(cfg.SharedProxy.Users) == 0 {
		return nil, fmt.Errorf("shared_proxy.tls_domain and shared_proxy.users are required in %s", path)
	}

	resolved := &resolvedRegistryConfig{
		RegistryConfig:     cfg,
		ProbeInterval:      time.Duration(cfg.Healthcheck.ProbeIntervalMs) * time.Millisecond,
		ProbeTimeout:       time.Duration(cfg.Healthcheck.ProbeTimeoutMs) * time.Millisecond,
		SelectionInterval:  time.Duration(cfg.Healthcheck.SelectionIntervalMs) * time.Millisecond,
		HeartbeatTTL:       time.Duration(cfg.Healthcheck.HeartbeatTTLSec) * time.Second,
		ReportFreshnessTTL: time.Duration(cfg.Healthcheck.ReportFreshnessMin) * time.Minute,
		PruneUnhealthyTTL:  time.Duration(resolvePruneUnhealthyMinutes(cfg.Healthcheck.PruneUnhealthyMin)) * time.Minute,
		PanelEnabled:       cfg.Panel.Enabled == nil || *cfg.Panel.Enabled,
		TerminateDeadTTL:   time.Duration(resolveIntDefault(cfg.Healthcheck.TerminateDeadMin, defaultTerminateDeadMinutes)) * time.Minute,
		QuarantineAttempts: max(1, cfg.Healthcheck.QuarantineAttempts), // дефолт 3 проставлен в applyRegistryDefaults
		EventsRetention:    time.Duration(cfg.Database.EventsRetentionDays) * 24 * time.Hour,
		DBEnabled:          cfg.Database.Enabled == nil || *cfg.Database.Enabled,
		configPath:         path,
	}
	return resolved, nil
}

// resolveIntDefault — семантика *int-полей: nil → дефолт, явное значение → оно
// (0 используется как «выключено» для TTL-полей).
func resolveIntDefault(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

func applyRegistryDefaults(cfg *RegistryConfig) {
	if cfg.HTTP.Addr == "" {
		cfg.HTTP.Addr = ":8080"
	}
	if cfg.State.File == "" {
		cfg.State.File = "registry_state.json"
	}
	if cfg.Healthcheck.ProbeIntervalMs == 0 {
		cfg.Healthcheck.ProbeIntervalMs = 5000
	}
	if cfg.Healthcheck.ProbeTimeoutMs == 0 {
		cfg.Healthcheck.ProbeTimeoutMs = 3000
	}
	if cfg.Healthcheck.SelectionIntervalMs == 0 {
		cfg.Healthcheck.SelectionIntervalMs = 3000
	}
	if cfg.Healthcheck.HeartbeatTTLSec == 0 {
		cfg.Healthcheck.HeartbeatTTLSec = 60
	}
	if cfg.Healthcheck.FailThreshold == 0 {
		cfg.Healthcheck.FailThreshold = 3
	}
	if cfg.Healthcheck.RecoverThreshold == 0 {
		cfg.Healthcheck.RecoverThreshold = 2
	}
	if cfg.Healthcheck.ReportFreshnessMin == 0 {
		cfg.Healthcheck.ReportFreshnessMin = 15
	}
	if cfg.NodeDefaults.HeartbeatMs == 0 {
		cfg.NodeDefaults.HeartbeatMs = 15000
	}
	if cfg.NodeDefaults.GlobalpingMs == 0 {
		cfg.NodeDefaults.GlobalpingMs = 300000
	}
	if cfg.NodeDefaults.MetricsMs == 0 {
		cfg.NodeDefaults.MetricsMs = 60000
	}
	if cfg.NodeDefaults.SyncMs == 0 {
		cfg.NodeDefaults.SyncMs = 60000
	}
	if cfg.Cloudflare.DNSTTL == 0 {
		cfg.Cloudflare.DNSTTL = 60
	}
	if cfg.Globalping.APIBase == "" {
		cfg.Globalping.APIBase = "https://api.globalping.io/v1"
	}
	if cfg.Panel.EventsMax == 0 {
		cfg.Panel.EventsMax = 500
	}
	// СРМД — лимит нод на домен. enabled по умолчанию ВЫКЛЮЧЕН
	// (nil → false), базовый домен разрешается лениво (первый из cloudflare).
	if cfg.SRMD.MaxNodesPerDomain == 0 {
		cfg.SRMD.MaxNodesPerDomain = defaultSRMDMaxNodesPerDomain
	}
	//
	if cfg.Healthcheck.QuarantineAttempts == 0 {
		cfg.Healthcheck.QuarantineAttempts = defaultQuarantineAttempts
	}
	if cfg.Database.EventsRetentionDays == 0 {
		cfg.Database.EventsRetentionDays = defaultEventsRetentionDays
	}
	if cfg.Database.Enabled == nil || *cfg.Database.Enabled {
		if cfg.Database.File == "" {
			// по умолчанию — рядом со state-файлом
			cfg.Database.File = defaultDBFileFor(cfg.State.File)
		}
	}
}
