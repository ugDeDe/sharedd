package agent

// Поддержка прокси mtdd (https://github.com/ugDeDe/mtdd).
//
// mtdd — самостоятельный C++-прокси: конфиг JSON (/etc/mtdd/config.json), без
// telemt.toml; управление и /metrics живут на одном локальном HTTP-порту под
// Bearer-токеном (/etc/mtdd/credentials.env, MTDD_API_TOKEN), пользователи —
// через /v1/users (секрет — 32 hex, как у telemt).
//
// Интеграция сведена к тонкому шву, чтобы остальной агент остался прежним:
//   - [telemt] config_path в node.toml указывает на JSON-конфиг mtdd
//     (isMtddConfigPath); loadTelemtConfig отдаёт из него порт прокси и адрес
//     /metrics в прежней структуре TelemtConfig — antiscan, globalping и
//     проверка общего порта работают без изменений;
//   - /metrics запрашивается с Bearer-токеном (metricsGet), а серии mtdd_*
//     переводятся в имена telemt_* (translateMtddSamples): health-критерий,
//     клиенты и трафик доезжают до регистратора теми же ключами;
//   - sync применяет общий конфиг не правкой TOML, а через API (пользователи,
//     без рестарта) и правкой tls_domain в JSON с рестартом mtdd.service и
//     откатом при неподъёме (applyMtddShared).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"sharedd/node/internal/config"
)

// Пути и имя юнита — var ради подмены в тестах.
var (
	mtddConfigPath = "/etc/mtdd/config.json"
	mtddCredsPath  = "/etc/mtdd/credentials.env"
	mtddUnit       = "mtdd.service"
	mtddBinary     = "/usr/local/bin/mtdd"
)

// mtddAPITimeout — таймаут одного вызова локального API mtdd.
const mtddAPITimeout = 5 * time.Second

// mtddConfig — используемое агентом подмножество /etc/mtdd/config.json.
type mtddConfig struct {
	ProxyPort   int    `json:"proxy_port"`
	ControlHost string `json:"control_host"`
	ControlPort int    `json:"control_port"`
	TLSDomain   string `json:"tls_domain"`
	UsersPath   string `json:"users_path"`
	// Новая схема токенов (ветка dev mtdd): admin — api_token_env, metrics —
	// отдельный слушатель metrics_host:metrics_port (явный порт > 0) с токеном
	// из metrics_token_env. Без явного metrics_port метрики берутся с порта
	// API под admin-токеном (так отвечают и прежние сборки).
	APITokenEnv     string `json:"api_token_env"`
	MetricsHost     string `json:"metrics_host"`
	MetricsPort     int    `json:"metrics_port"`
	MetricsTokenEnv string `json:"metrics_token_env"`
}

// isMtddConfigPath — [telemt] config_path указывает на конфиг mtdd (JSON).
// telemt.toml и config.toml MTProxyL JSON-ом быть не могут.
func isMtddConfigPath(path string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(path)), ".json")
}

func loadMtddConfig(path string) (*mtddConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read mtdd config at %s: %w", path, err)
	}
	var c mtddConfig
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("failed to parse mtdd config: %w", err)
	}
	if c.ControlPort == 0 {
		c.ControlPort = 9090
	}
	if strings.TrimSpace(c.ControlHost) == "" {
		c.ControlHost = "127.0.0.1"
	}
	if strings.TrimSpace(c.APITokenEnv) == "" {
		c.APITokenEnv = "MTDD_API_TOKEN"
	}
	return &c, nil
}

// asTelemt — представление конфига mtdd в структуре TelemtConfig: порт
// клиентского прокси и адрес /metrics (тот же control-порт).
func (c *mtddConfig) asTelemt() *TelemtConfig {
	var t TelemtConfig
	t.Server.Port = c.ProxyPort
	host, port := strings.Trim(c.ControlHost, "[]"), c.ControlPort
	if c.splitMetrics() {
		host, port = strings.Trim(strings.TrimSpace(c.MetricsHost), "[]"), c.MetricsPort
		if host == "" {
			host = "127.0.0.1"
		}
	}
	t.Server.MetricsPort = port
	t.Server.MetricsListen = net.JoinHostPort(host, strconv.Itoa(port))
	return &t
}

// splitMetrics — /metrics отдаётся отдельным слушателем с metrics-токеном.
func (c *mtddConfig) splitMetrics() bool {
	return c.MetricsPort > 0 && strings.TrimSpace(c.MetricsTokenEnv) != ""
}

// mtddBase — базовый URL локального API ("http://127.0.0.1:9090").
func (c *mtddConfig) base() string {
	host := strings.Trim(strings.TrimSpace(c.ControlHost), "[]")
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(c.ControlPort))
}

var mtddEnvLineRe = regexp.MustCompile(`^(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)=["']?([A-Za-z0-9]{32,512})["']?\s*$`)

// mtddEnvToken — значение токена из credentials.env по имени переменной (читает
// только root; наружу не логируется).
func mtddEnvToken(name string) (string, error) {
	data, err := os.ReadFile(mtddCredsPath)
	if err != nil {
		return "", fmt.Errorf("read mtdd credentials: %w", err)
	}
	for _, ln := range strings.Split(string(data), "\n") {
		if m := mtddEnvLineRe.FindStringSubmatch(strings.TrimSpace(ln)); m != nil && m[1] == name {
			return m[2], nil
		}
	}
	return "", fmt.Errorf("no valid %s in %s", name, mtddCredsPath)
}

// adminToken — токен управления (api_token_env).
func (c *mtddConfig) adminToken() (string, error) { return mtddEnvToken(c.APITokenEnv) }

// metricsToken — токен для /metrics: отдельный metrics-токен при выделенном
// слушателе, иначе admin (метрики на порту API).
func (c *mtddConfig) metricsToken() (string, error) {
	if c.splitMetrics() {
		return mtddEnvToken(strings.TrimSpace(c.MetricsTokenEnv))
	}
	return c.adminToken()
}

// metricsGet — GET /metrics; на mtdd-ноде с Bearer-токеном.
func metricsGet(client *http.Client, cfg *config.NodeConfig, target string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	if isMtddConfigPath(cfg.Telemt.ConfigPath) {
		mc, merr := loadMtddConfig(cfg.Telemt.ConfigPath)
		if merr != nil {
			return nil, merr
		}
		tok, terr := mc.metricsToken()
		if terr != nil {
			return nil, terr
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return client.Do(req)
}

// translateMtddSamples переводит серии mtdd_* в имена, которые ждёт
// buildMetricsSnapshot (telemt_*). Ключевая метрика здоровья —
// сумма mtdd_me_pool_ready по DC (готовые ME-соединения); если серии пула нет
// вообще, telemt_me_writers_active_current не эмитится — «метрика не найдена»
// отличается от «0 готовых».
//
// Уникальные IP: аналог telemt_user_unique_ips_current — накопленные
// mtdd_user_unique_ips_seen (active — только текущие соединения, заметно
// меньше). Старый mtdd без _seen — fallback на _active.
func translateMtddSamples(in []promSample) []promSample {
	out := make([]promSample, 0, len(in))
	var ready, reconnects float64
	var readySeen, reconnSeen bool
	var ipsSeen, ipsActive []promSample
	for _, s := range in {
		switch s.name {
		case "mtdd_me_pool_ready":
			ready += s.value
			readySeen = true
		case "mtdd_me_reconnects_total":
			reconnects += s.value
			reconnSeen = true
		case "mtdd_user_unique_ips_seen":
			ipsSeen = append(ipsSeen, promSample{name: uniqueIPsMetricName, labels: s.labels, value: s.value})
		case "mtdd_user_unique_ips_active":
			ipsActive = append(ipsActive, promSample{name: uniqueIPsMetricName, labels: s.labels, value: s.value})
		case "mtdd_user_connections_active":
			out = append(out, promSample{name: userConnsMetricName, labels: s.labels, value: s.value})
		case "mtdd_user_payload_bytes_received_total":
			user := promUserLabel(s.labels)
			if user == "" {
				continue
			}
			labels := `user="` + user + `"`
			switch {
			case strings.Contains(s.labels, `direction="c2s"`):
				out = append(out, promSample{name: userOctetsFromMetricName, labels: labels, value: s.value})
			case strings.Contains(s.labels, `direction="s2c"`):
				out = append(out, promSample{name: userOctetsToMetricName, labels: labels, value: s.value})
			}
		}
	}
	if len(ipsSeen) > 0 {
		out = append(out, ipsSeen...)
	} else {
		out = append(out, ipsActive...)
	}
	if readySeen {
		out = append(out, promSample{name: healthMetricName, value: ready})
	}
	if reconnSeen {
		out = append(out, promSample{name: "telemt_me_reconnect_attempts_total", value: reconnects})
	}
	return out
}

// ---- применение общего конфига ----

// mtddUserRecord — запись users.json (секрет читаем с диска: API его не отдаёт).
type mtddUserRecord struct {
	Name      string `json:"name"`
	Secret    string `json:"secret"`
	Enabled   bool   `json:"enabled"`
	TLSDomain string `json:"tls_domain"` // персональный SNI (mtdd с поддержкой tls_domain)
}

var mtddSecretRe = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)

// mtddUsersFile — путь users.json из конфига (относительный — от /etc/mtdd).
func mtddUsersFile(c *mtddConfig) string {
	p := strings.TrimSpace(c.UsersPath)
	if p == "" {
		p = "/var/lib/mtdd/users.json"
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(filepath.Dir(mtddConfigPath), p)
	}
	return p
}

func readMtddUsers(c *mtddConfig) (map[string]mtddUserRecord, error) {
	data, err := os.ReadFile(mtddUsersFile(c))
	if err != nil {
		return nil, err
	}
	var doc struct {
		Users []mtddUserRecord `json:"users"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	m := make(map[string]mtddUserRecord, len(doc.Users))
	for _, u := range doc.Users {
		m[u.Name] = u
	}
	return m, nil
}

// mtddAPI — вызов локального API mtdd; тело ответа ограничено.
func mtddAPI(c *mtddConfig, method, path string, body any) error {
	tok, err := c.adminToken()
	if err != nil {
		return err
	}
	var rdr io.Reader
	if body != nil {
		raw, merr := json.Marshal(body)
		if merr != nil {
			return merr
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base()+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: mtddAPITimeout}).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// mtddUnknownField — API отклонил поле тела (прежние сборки mtdd без
// персонального tls_domain отвечают 400 unknown_field).
func mtddUnknownField(err error) bool {
	return err != nil && strings.Contains(err.Error(), "unknown_field")
}

// syncMtddUsers приводит пользователей mtdd к общему конфигу регистратора:
// отсутствующих создаёт, с чужим секретом ротирует, выключенных включает и
// выставляет каждому общему пользователю персональный SNI (tls_domain) — смена
// SNI не требует ни правки конфига, ни рестарта. Если домен совпадает с
// глобальным tls_domain, персональное значение снимается. Локальных
// пользователей не удаляет и не трогает. Возвращает число изменений и признак
// sniUnsupported (mtdd без поля tls_domain: SNI надо менять в конфиге);
// ошибки отдельных пользователей не прерывают остальных.
func syncMtddUsers(c *mtddConfig, shared map[string]string, domain string) (changed int, sniUnsupported bool, err error) {
	existing, rerr := readMtddUsers(c)
	if rerr != nil && !os.IsNotExist(rerr) {
		log.Printf("mtdd: cannot read users file %s: %v (creating missing users only)", mtddUsersFile(c), rerr)
	}
	// секрет уже у другого пользователя — mtdd запрещает дубликаты (409), а
	// клиенты с этим секретом и так обслуживаются: пропускаем.
	holder := make(map[string]string, len(existing))
	for _, u := range existing {
		holder[strings.ToLower(u.Secret)] = u.Name
	}
	wantSNI := strings.ToLower(strings.TrimSpace(domain))
	if wantSNI == strings.ToLower(c.TLSDomain) {
		wantSNI = "" // глобальный домен — персональное значение не нужно
	}
	var firstErr error
	note := func(e error) {
		if e != nil && firstErr == nil {
			firstErr = e
		}
	}
	for name, secret := range shared {
		if !mtddSecretRe.MatchString(secret) {
			log.Printf("mtdd: skip user %q: shared secret is not 32 hex characters", name)
			continue
		}
		secret = strings.ToLower(secret)
		enc := url.PathEscape(name)
		cur, ok := existing[name]
		if h, dup := holder[secret]; dup && h != name {
			if _, stillShared := shared[h]; stillShared {
				continue // секрет делят два общих имени — mtdd такое не принимает
			}
			// Переименование: тот же секрет у пользователя, которого в общем
			// конфиге больше нет. Удаляем старое имя и создаём новое — иначе
			// онлайн считается под старым именем и не доходит до регистратора.
			if e := mtddAPI(c, http.MethodDelete, "/v1/users/"+url.PathEscape(h), nil); e != nil {
				log.Printf("mtdd: remove renamed user %q: %v", h, e)
				note(e)
				continue
			}
			log.Printf("mtdd: user %q renamed to %q (same secret): old name removed", h, name)
			delete(existing, h)
			delete(holder, secret)
			changed++
			ok = false
		}
		if !ok {
			body := map[string]any{"name": name, "secret": secret}
			if wantSNI != "" && !sniUnsupported {
				body["tls_domain"] = wantSNI
			}
			e := mtddAPI(c, http.MethodPost, "/v1/users", body)
			if mtddUnknownField(e) && body["tls_domain"] != nil {
				sniUnsupported = true
				delete(body, "tls_domain")
				e = mtddAPI(c, http.MethodPost, "/v1/users", body)
			}
			if e != nil && rerr != nil {
				// Без чтения users.json «нет в списке» неизвестно: пользователь
				// мог уже существовать — тогда ротируем секрет.
				e = mtddAPI(c, http.MethodPost, "/v1/users/"+enc+"/rotate-secret", map[string]any{"secret": secret})
			}
			if e != nil {
				log.Printf("mtdd: create user %q: %v", name, e)
				note(e)
				continue
			}
			log.Printf("mtdd: synchronized user %q", name)
			changed++
			continue
		}
		if !strings.EqualFold(cur.Secret, secret) {
			if e := mtddAPI(c, http.MethodPost, "/v1/users/"+enc+"/rotate-secret", map[string]any{"secret": secret}); e != nil {
				log.Printf("mtdd: rotate secret for %q: %v", name, e)
				note(e)
				continue
			}
			log.Printf("mtdd: synchronized secret for user %q", name)
			changed++
		}
		patch := map[string]any{}
		if !cur.Enabled {
			patch["enabled"] = true
		}
		if domain != "" && !sniUnsupported && !strings.EqualFold(cur.TLSDomain, wantSNI) {
			patch["tls_domain"] = wantSNI // "" снимает персональный домен
		}
		if len(patch) == 0 {
			continue
		}
		e := mtddAPI(c, http.MethodPatch, "/v1/users/"+enc, patch)
		if mtddUnknownField(e) && patch["tls_domain"] != nil {
			sniUnsupported = true
			delete(patch, "tls_domain")
			e = nil
			if len(patch) > 0 {
				e = mtddAPI(c, http.MethodPatch, "/v1/users/"+enc, patch)
			}
		}
		if e != nil {
			log.Printf("mtdd: update user %q: %v", name, e)
			note(e)
			continue
		}
		if len(patch) > 0 {
			log.Printf("mtdd: updated user %q (%s)", name, patchKeys(patch))
			changed++
		}
	}
	return changed, sniUnsupported, firstErr
}

func patchKeys(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// setMtddTLSDomain пишет tls_domain в конфиг mtdd (остальные ключи сохраняются,
// числа — без потери точности). Возвращает исходный текст для отката и признак
// изменения.
func setMtddTLSDomain(path, domain string) (orig []string, changed bool, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", path, err)
	}
	if cur, _ := doc["tls_domain"].(string); cur == domain {
		return nil, false, nil
	}
	doc["tls_domain"] = domain
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, false, err
	}
	out = append(out, '\n')
	if err := writeLines(path, strings.Split(string(out), "\n")); err != nil {
		return nil, false, err
	}
	return strings.Split(string(raw), "\n"), true, nil
}

// mtddCheckConfig — best-effort проверка конфига бинарником (--check-config)
// до рестарта; без бинарника проверка пропускается.
func mtddCheckConfig(path string) error {
	if _, err := os.Stat(mtddBinary); err != nil {
		return nil
	}
	out, err := exec.Command(mtddBinary, "--config", path, "--check-config").CombinedOutput()
	if err != nil {
		return fmt.Errorf("mtdd --check-config: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// applyMtddShared — применение /config на mtdd-ноде целиком через API:
// пользователи и их персональный SNI меняются без рестарта и правки конфига.
// Запасной путь только для прежних сборок mtdd без поля tls_domain: правка
// tls_domain в JSON, check-config, рестарт mtdd.service, ожидание /metrics и
// откат файла при неподъёме. Вызывается под applySvcMu.
func applyMtddShared(cfg *config.NodeConfig, shared SharedConfig) error {
	path := cfg.Telemt.ConfigPath
	c, err := loadMtddConfig(path)
	if err != nil {
		return err
	}

	var firstErr error
	sniUnsupported := false
	if len(shared.Users) > 0 {
		var uerr error
		_, sniUnsupported, uerr = syncMtddUsers(c, shared.Users, shared.TLSDomain)
		if uerr != nil {
			firstErr = fmt.Errorf("mtdd users sync: %w", uerr)
		}
	}

	if !sniUnsupported || shared.TLSDomain == "" || strings.EqualFold(c.TLSDomain, shared.TLSDomain) {
		return firstErr
	}
	log.Printf("mtdd: API without per-user tls_domain — falling back to config edit + restart")

	orig, changed, werr := setMtddTLSDomain(path, shared.TLSDomain)
	if werr != nil {
		return fmt.Errorf("set tls_domain in %s: %w", path, werr)
	}
	if !changed {
		return firstErr
	}
	log.Printf("mtdd: tls_domain in %s: %q -> %q", path, c.TLSDomain, shared.TLSDomain)

	restart := func() error { return applyProxyCtl("restart", mtddUnit) }
	if cerr := mtddCheckConfig(path); cerr != nil {
		_ = writeLines(path, orig)
		return fmt.Errorf("new mtdd config rejected (rolled back): %w", cerr)
	}
	if !applySystemdAvailable() {
		_ = writeLines(path, orig)
		return fmt.Errorf("no systemd: cannot restart %s (config rolled back)", mtddUnit)
	}
	if rerr := restart(); rerr != nil {
		return rollbackConfig(path, orig, restart, fmt.Errorf("restart %s after config write: %w", mtddUnit, rerr))
	}
	if werr := applyWaitMetrics(cfg, proxyUpTimeout); werr != nil {
		return rollbackConfig(path, orig, restart, fmt.Errorf("mtdd not ready after config apply: %w", werr))
	}
	log.Printf("mtdd: %s is up with new tls_domain", mtddUnit)
	return firstErr
}
