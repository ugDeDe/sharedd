package main

import (
	"crypto/rand"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// Зашитые дефолты (бесконфиговая нода — настраивается только registry.url,
// telemt.config_path и sync.apply_to_telemt).
const (
	defaultTelemtConfigPath   = "/etc/telemt/telemt.toml"
	defaultIDStateFile        = "/var/lib/sharedd/node_id"
	healthMetricName          = "telemt_me_writers_active_current"
	uniqueIPsMetricName       = "telemt_user_unique_ips_current"
	userConnsMetricName       = "telemt_user_connections_current"
	userOctetsFromMetricName  = "telemt_user_octets_from_client"
	userOctetsToMetricName    = "telemt_user_octets_to_client"
	trafficIngressMetricName  = "sharedd_traffic_ingress_bytes_total"
	trafficEgressMetricName   = "sharedd_traffic_egress_bytes_total"
	trafficUsersMetricName    = "sharedd_traffic_users_fingerprint"
	globalpingAPIBase         = "https://api.globalping.io/v1"
	metricsListenKey          = "metrics_listen"
	metricsListenValue        = "127.0.0.1:9090"
	metricsPortKey            = "metrics_port"
	defaultIntervalsHeartbeat = 15000
	defaultIntervalsGlobal    = 300000
	defaultIntervalsMetrics   = 60000
	defaultIntervalsSync      = 60000
)

type NodeConfig struct {
	Registry struct {
		URL   string `toml:"url"`
		Token string `toml:"token"`
	} `toml:"registry"`

	Telemt struct {
		ConfigPath string `toml:"config_path"`
		// Режим mode/auto/file/api УДАЛЁН — интеграция только через файл
		// telemt.toml (telemt REST API не используется). Ключ mode в старых
		// конфигах просто игнорируется (toml пропускает неизвестные поля).
	} `toml:"telemt"`

	Sync struct {
		ApplyToTelemt bool `toml:"apply_to_telemt"`
	} `toml:"sync"`

	// Watchdog: dead_kill_ms — непрерывно красные локальные
	// проверки (scrape telemt /metrics) дольше этого окна → терминальное
	// само-завершение ноды (класс dead, см. terminate.go): агент пишет в лог
	// msgDead, кладёт tombstone, шлёт /retire регистратору и останавливает
	// свою службу. 0/отсутствует = 600000 (10 мин); -1 = выключено.
	Watchdog struct {
		DeadKillMs int `toml:"dead_kill_ms"`
	} `toml:"watchdog"`

	// Node: public_ip — ручное задание публичного IPv4, если
	// авто-детект недоступен/непригоден (жёсткий DNAT, hairpin NAT,
	// вывешенный адрес не совпадает с исходящим). Пусто = авто-детект.
	Node struct {
		PublicIP string `toml:"public_ip"`
	} `toml:"node"`

	// Globalping: api_base — переопределение API (совместимое
	// зеркало/прокси; как [globalping] api_base у регистратора).
	Globalping struct {
		APIBase string `toml:"api_base"`
	} `toml:"globalping"`
}

// deadKill — эффективное окно dead-килла (0 = выключено).
func (c *NodeConfig) deadKill() time.Duration {
	if c.Watchdog.DeadKillMs < 0 {
		return 0
	}
	if c.Watchdog.DeadKillMs == 0 {
		return 10 * time.Minute
	}
	return time.Duration(c.Watchdog.DeadKillMs) * time.Millisecond
}

func nodeConfigPathFlag() string {
	fs := flag.NewFlagSet("node", flag.ContinueOnError)
	path := fs.String("config", "", "path to node agent TOML config file")
	// объявлен и здесь, чтобы парсер не ругался на неизвестный флаг
	// (сам флаг читается applyOnceFlag() ручным сканом os.Args)
	_ = fs.Bool("apply-once", false, "apply shared config once (stop->patch->start->wait) and exit")
	_ = fs.Parse(os.Args[1:])
	if *path != "" {
		return *path
	}
	if v := os.Getenv("NODE_CONFIG_PATH"); v != "" {
		return v
	}
	return "/etc/sharedd/node.toml"
}

// applyOnceFlag — режим one-shot конвейера: -apply-once / --apply-once.
// Сканируем os.Args ВРУЧНУЮ: nodeConfigPathFlag уже владеет своим FlagSet'ом, а
// второй FlagSet по тем же аргументам оборвётся на первом неизвестном флаге.
func applyOnceFlag() bool {
	for _, a := range os.Args[1:] {
		if a == "-apply-once" || a == "--apply-once" {
			return true
		}
	}
	return false
}

func loadNodeConfig() (*NodeConfig, error) {
	path := nodeConfigPathFlag()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config %s: %w", path, err)
	}

	var cfg NodeConfig
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config %s: %w", path, err)
	}

	if strings.TrimSpace(cfg.Registry.URL) == "" {
		return nil, fmt.Errorf("registry.url is required in %s", path)
	}
	if strings.TrimSpace(cfg.Registry.Token) == "" {
		return nil, fmt.Errorf("registry.token is required in %s", path)
	}
	cfg.Registry.URL = strings.TrimRight(cfg.Registry.URL, "/")
	if err := validateHTTPURL(cfg.Registry.URL); err != nil {
		return nil, fmt.Errorf("registry.url in %s: %w", path, err)
	}
	if cfg.Watchdog.DeadKillMs < -1 || cfg.Watchdog.DeadKillMs > int((30*24*time.Hour)/time.Millisecond) {
		return nil, fmt.Errorf("watchdog.dead_kill_ms in %s must be -1, 0, or at most 30 days", path)
	}
	if cfg.Node.PublicIP != "" && (net.ParseIP(cfg.Node.PublicIP) == nil || net.ParseIP(cfg.Node.PublicIP).To4() == nil) {
		return nil, fmt.Errorf("node.public_ip in %s is invalid", path)
	}
	if cfg.Telemt.ConfigPath == "" {
		cfg.Telemt.ConfigPath = defaultTelemtConfigPath
	}
	if cfg.Globalping.APIBase == "" {
		cfg.Globalping.APIBase = globalpingAPIBase
	}
	if err := validateHTTPURL(cfg.Globalping.APIBase); err != nil {
		return nil, fmt.Errorf("globalping.api_base in %s: %w", path, err)
	}
	return &cfg, nil
}

func validateHTTPURL(raw string) error {
	u, err := url.ParseRequestURI(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("must be an absolute http(s) URL")
	}
	return nil
}

var nodeIDPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]{0,8}[A-Za-z0-9])?-[a-z0-9]{5}$`)

// resolveNodeID — случайный персистентный ID с коротким читаемым именем.
// ID генерируется один раз и сохраняется в state-файл: он определяет место ноды
// в очереди регистратора (RegisteredAt), поэтому не должен меняться на рестартах.
func resolveNodeID() (string, error) {
	return loadOrGenerateRandomID(defaultIDStateFile)
}

func loadOrGenerateRandomID(stateFile string) (string, error) {
	name := "node"
	if data, err := os.ReadFile(stateFile); err == nil {
		id := strings.TrimSpace(string(data))
		if nodeIDPattern.MatchString(id) {
			return id, nil
		}
		name = nodeNameFromLegacyID(id)
	}

	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	buf := make([]byte, 5)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("crypto/rand read failed: %w", err)
	}
	for i := range buf {
		buf[i] = alphabet[int(buf[i])%len(alphabet)]
	}
	id := name + "-" + string(buf)

	if err := os.MkdirAll(filepath.Dir(stateFile), 0700); err != nil {
		return "", fmt.Errorf("failed to create dir for %s: %w", stateFile, err)
	}
	if err := os.WriteFile(stateFile, []byte(id), 0600); err != nil {
		return "", fmt.Errorf("failed to persist random node id to %s: %w", stateFile, err)
	}

	return id, nil
}

func nodeNameFromLegacyID(id string) string {
	if i := strings.LastIndexByte(id, '-'); i > 0 {
		id = id[:i]
	}
	var b strings.Builder
	for _, r := range id {
		if b.Len() >= 10 {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		}
	}
	name := strings.Trim(b.String(), "._-")
	if name == "" {
		return "node"
	}
	return name
}
