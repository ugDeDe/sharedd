package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// ---- telemt config ----

type TelemtConfig struct {
	Server struct {
		Port          int    `toml:"port"`
		MetricsPort   int    `toml:"metrics_port"`
		MetricsListen string `toml:"metrics_listen"`
	} `toml:"server"`
}

func loadTelemtConfig(path string) (*TelemtConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read telemt config at %s: %w", path, err)
	}
	var cfg TelemtConfig
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse telemt config: %w", err)
	}
	return &cfg, nil
}

func telemtProxyPort(cfg *TelemtConfig) int {
	if cfg.Server.Port != 0 {
		return cfg.Server.Port
	}
	return 443
}

// metricsURLFromTelemt собирает URL метрик из telemt.toml.
// Метрика всегда имеет вид http://<host>:<port>/metrics (по умолчанию локальный).
// Приоритет: metrics_listen ("host:port") > metrics_port > 127.0.0.1:9090.
func metricsURLFromTelemt(metricsPort int, metricsListen string) string {
	host := "127.0.0.1"
	port := metricsPort
	if port == 0 {
		port = 9090
	}
	if ml := strings.TrimSpace(metricsListen); ml != "" {
		if h, p, err := net.SplitHostPort(ml); err == nil {
			host = h
			if parsed, perr := strconv.Atoi(p); perr == nil && parsed > 0 {
				port = parsed
			}
		} else {
			host = ml // голый хост без порта — берём port как есть
		}
		host = strings.Trim(host, "[]")
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/metrics"
}

// ---- globalping ----

type globalpingMeasurementRequest struct {
	Type               string                    `json:"type"`
	Target             string                    `json:"target"`
	MeasurementOptions globalpingMeasurementOpts `json:"measurementOptions"`
	Locations          []globalpingLocation      `json:"locations"`
	Limit              int                       `json:"limit"`
}

type globalpingMeasurementOpts struct {
	Protocol string                `json:"protocol"`
	Port     int                   `json:"port"`
	Request  globalpingHTTPRequest `json:"request"`
}

type globalpingHTTPRequest struct {
	Method string `json:"method"`
	Host   string `json:"host"`
}

type globalpingLocation struct {
	Country string   `json:"country"`
	Tags    []string `json:"tags,omitempty"`
}

type globalpingCreateResponse struct {
	ID          string `json:"id"`
	ProbesCount int    `json:"probesCount"`
}

type globalpingProbeResult struct {
	Status     string `json:"status"`
	StatusCode int    `json:"statusCode"`
}

type globalpingProbeMeasurement struct {
	Result globalpingProbeResult `json:"result"`
}

type globalpingMeasurement struct {
	ID      string                       `json:"id"`
	Status  string                       `json:"status"`
	Results []globalpingProbeMeasurement `json:"results"`
}

type GlobalpingChecker struct {
	APIBase string
	Client  *http.Client
}

func NewGlobalpingChecker(apiBase string) *GlobalpingChecker {
	if apiBase == "" {
		apiBase = globalpingAPIBase
	}
	return &GlobalpingChecker{APIBase: apiBase, Client: &http.Client{Timeout: 15 * time.Second}}
}

func (g *GlobalpingChecker) CreateAndAwait(ip string, port int, fakeSNI string) (string, float64, error) {
	reqBody := globalpingMeasurementRequest{
		Type:   "http",
		Target: ip,
		MeasurementOptions: globalpingMeasurementOpts{
			Protocol: "HTTPS",
			Port:     port,
			Request: globalpingHTTPRequest{
				Method: "HEAD",
				Host:   fakeSNI,
			},
		},
		Locations: []globalpingLocation{{Country: "RU", Tags: []string{"eyeball-network"}}},
		Limit:     20,
	}
	data, _ := json.Marshal(reqBody)

	resp, err := g.Client.Post(g.APIBase+"/measurements", "application/json", bytes.NewReader(data))
	if err != nil {
		return "", 0, fmt.Errorf("globalping create error: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", 0, fmt.Errorf("globalping create failed: status=%d body=%s", resp.StatusCode, string(body))
	}
	var created globalpingCreateResponse
	if err := json.Unmarshal(body, &created); err != nil {
		return "", 0, fmt.Errorf("globalping create parse error: %w", err)
	}
	if created.ID == "" {
		return "", 0, fmt.Errorf("globalping did not return measurement id: %s", string(body))
	}

	measurement, err := g.pollUntilDone(created.ID, 45*time.Second)
	if err != nil {
		return created.ID, 0, err
	}
	return created.ID, evaluateSuccessRatio(measurement), nil
}

func (g *GlobalpingChecker) pollUntilDone(id string, timeout time.Duration) (*globalpingMeasurement, error) {
	deadline := time.Now().Add(timeout)
	for {
		m, err := g.FetchMeasurement(id)
		if err != nil {
			return nil, err
		}
		// Top-level статус по Globalping OpenAPI — только "in-progress"/"finished";
		// "failed"/"offline" оставлены для устойчивости к вариациям API
		// (per-probe result.status такими бывает).
		if m.Status == "finished" || m.Status == "failed" || m.Status == "offline" {
			return m, nil
		}
		if time.Now().After(deadline) {
			return m, fmt.Errorf("globalping measurement %s did not finish in time", id)
		}
		time.Sleep(2 * time.Second)
	}
}

func (g *GlobalpingChecker) FetchMeasurement(id string) (*globalpingMeasurement, error) {
	resp, err := g.Client.Get(g.APIBase + "/measurements/" + id)
	if err != nil {
		return nil, fmt.Errorf("globalping fetch error: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("globalping fetch failed: status=%d body=%s", resp.StatusCode, string(body))
	}
	var m globalpingMeasurement
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("globalping fetch parse error: %w", err)
	}
	return &m, nil
}

func evaluateSuccessRatio(m *globalpingMeasurement) float64 {
	if m == nil || len(m.Results) == 0 {
		return 0
	}
	success := 0
	for _, r := range m.Results {
		if r.Result.Status == "finished" && r.Result.StatusCode >= 200 && r.Result.StatusCode < 400 {
			success++
		}
	}
	return float64(success) / float64(len(m.Results))
}

// ---- metrics ----

// promSample — одна серия Prometheus text exposition: имя, labels (содержимое
// {...}, может быть пустым) и значение. Labels сохраняются, потому что per-user
// метрики telemt (telemt_user_unique_ips_current{user="x"}) иначе схлопываются
// в одну серию "последняя победила".
type promSample struct {
	name   string
	labels string
	value  float64
}

func parsePrometheusSamples(text string) []promSample {
	var out []promSample
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var name, labels, rest string
		if i := strings.IndexByte(line, '{'); i >= 0 {
			j := strings.LastIndexByte(line, '}')
			if j < i {
				continue
			}
			name = line[:i]
			labels = line[i+1 : j]
			rest = strings.TrimSpace(line[j+1:])
		} else {
			sp := strings.IndexAny(line, " \t")
			if sp < 0 {
				continue
			}
			name = line[:sp]
			rest = strings.TrimSpace(line[sp:])
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		val, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			continue
		}
		out = append(out, promSample{name: name, labels: labels, value: val})
	}
	return out
}

// snapshotMaxUserSeries — верхняя граница per-user серий в снапшоте: защита
// state-файла регистратора (он персистит снапшот целиком) от безумных конфигов.
const snapshotMaxUserSeries = 64

// promUserLabel — значение label'а user из строки labels prom-серии
// (`user="u0cb271"` → «u0cb271», `user="a",x="b"` → «a»). Пусто — label'а
// нет или он не распознан.
func promUserLabel(labels string) string {
	for _, part := range strings.Split(labels, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 || strings.TrimSpace(kv[0]) != "user" {
			continue
		}
		return strings.Trim(strings.TrimSpace(kv[1]), `"`)
	}
	return ""
}

// buildMetricsSnapshot собирает снимок метрик для регистратора.
// Возвращает сам снапшот и плоскую карту name→value (для health-гейта).
//
// Ключ uniqueIPsMetricName (без labels) — СУММА per-user серий
// telemt_user_unique_ips_current{user="..."}: уникальные активные клиентские IP
// ноды. Активные клиенты считаются ТОЛЬКО ПО ОБЩЕМУ СЕКРЕТУ — в
// сумму входят лишь серии тех пользователей, которых раздаёт регистратор
// (shared_proxy.users, приходят в /config и живут в sharedConfigCache);
// локальные юзеры ноды (чужие записи [access.users], которые мы никогда не
// перезаписываем) в агрегат НЕ входят. sharedUsers пуст (конфиг ещё не
// приехал с регистратора) — fallback на сумму всех серий, чтобы не рисовать
// «нет данных» до первого sync. Один IP, гоняющий несколько общих secret'ов,
// посчитается по разу на каждого пользователя (приемлемая аппроксимация,
// дедупа по IP в telemt metrics нет). Аналогично userConnsMetricName — сумма
// активных клиентских подключений. Сами per-user серии тоже кладём
// (ключи вида name{user="x"}) — регистратор их не трогает, но по /status видно.
//
// Агрегаты пишем ТОЛЬКО если подходящие per-user серии вообще встретились в
// выдаче: при [general.telemetry] user_enabled=false telemt их не эмитит, и
// мы обязаны отличать "нет данных" (ключа нет → панель рисует "—") от
// "0 клиентов".
func buildMetricsSnapshot(samples []promSample, sharedUsers map[string]string) (snapshot, values map[string]float64) {
	values = make(map[string]float64, len(samples))
	for _, s := range samples {
		values[s.name] = s.value
	}
	snapshot = map[string]float64{
		healthMetricName:                        values[healthMetricName],
		"telemt_upstream_connect_attempt_total": values["telemt_upstream_connect_attempt_total"],
		"telemt_me_reconnect_attempts_total":    values["telemt_me_reconnect_attempts_total"],
	}
	// общий секрет: пользователь входит в счёт, только если его раздаёт
	// регистратор; пустой кэш = конфиг ещё не загружен → считаем всех
	counted := func(user string) bool {
		if len(sharedUsers) == 0 {
			return true
		}
		_, ok := sharedUsers[user]
		return ok
	}
	var uniqSum, connSum, ingressSum, egressSum float64
	var uniqSeen, connSeen, ingressSeen, egressSeen bool
	userSeries := 0
	for _, s := range samples {
		switch s.name {
		case uniqueIPsMetricName:
			if !counted(promUserLabel(s.labels)) {
				continue // локальный (не общий) пользователь — в счёт не входит
			}
			uniqSum += s.value
			uniqSeen = true
			if s.labels != "" && userSeries < snapshotMaxUserSeries {
				snapshot[s.name+"{"+s.labels+"}"] = s.value
				userSeries++
			}
		case userConnsMetricName:
			if !counted(promUserLabel(s.labels)) {
				continue
			}
			connSum += s.value
			connSeen = true
		case userOctetsFromMetricName:
			if counted(promUserLabel(s.labels)) {
				ingressSum += s.value
				ingressSeen = true
			}
		case userOctetsToMetricName:
			if counted(promUserLabel(s.labels)) {
				egressSum += s.value
				egressSeen = true
			}
		}
	}
	if uniqSeen {
		snapshot[uniqueIPsMetricName] = uniqSum
	}
	if connSeen {
		snapshot[userConnsMetricName] = connSum
	}
	if ingressSeen {
		snapshot[trafficIngressMetricName] = ingressSum
	}
	if egressSeen {
		snapshot[trafficEgressMetricName] = egressSum
	}
	if ingressSeen || egressSeen {
		snapshot[trafficUsersMetricName] = trafficUsersFingerprint(sharedUsers)
	}
	return snapshot, values
}

func trafficUsersFingerprint(users map[string]string) float64 {
	names := make([]string, 0, len(users))
	for name := range users {
		names = append(names, name)
	}
	sort.Strings(names)
	h := fnv.New64a()
	for _, name := range names {
		_, _ = h.Write([]byte(name))
		_, _ = h.Write([]byte{0})
	}
	return float64(h.Sum64() & ((1 << 53) - 1))
}

// ---- health reports ----

type HealthReport struct {
	NodeID                  string             `json:"node_id"`
	IP                      string             `json:"ip"`
	Port                    int                `json:"port"`
	FakeSNI                 string             `json:"fake_sni"`
	GlobalpingOK            bool               `json:"globalping_ok"`
	GlobalpingMeasurementID string             `json:"globalping_measurement_id"`
	GlobalpingSuccessRatio  float64            `json:"globalping_success_ratio"`
	MetricsOK               bool               `json:"metrics_ok"`
	MetricsSnapshot         map[string]float64 `json:"metrics_snapshot,omitempty"`
	Healthy                 bool               `json:"healthy"`
	CheckedAt               time.Time          `json:"checked_at"`
	Error                   string             `json:"error,omitempty"`
}

// lastMetrics — кэш последнего результата metrics-цикла; globalping-цикл
// прикладывает его к своему отчёту, чтобы регистратор всегда видел обе половины.
var lastMetrics = struct {
	sync.RWMutex
	ok       bool
	snapshot map[string]float64
	checked  bool
}{}

func cacheMetrics(ok bool, snapshot map[string]float64) {
	lastMetrics.Lock()
	lastMetrics.ok, lastMetrics.snapshot, lastMetrics.checked = ok, snapshot, true
	lastMetrics.Unlock()
}

func getCachedMetrics() (ok bool, snapshot map[string]float64, checked bool) {
	lastMetrics.RLock()
	defer lastMetrics.RUnlock()
	return lastMetrics.ok, lastMetrics.snapshot, lastMetrics.checked
}

func baseReport(nodeID, ip string, telemtCfg *TelemtConfig) HealthReport {
	r := HealthReport{
		NodeID:    nodeID,
		IP:        ip,
		CheckedAt: time.Now(),
	}
	if telemtCfg != nil {
		r.Port = telemtProxyPort(telemtCfg)
	}
	if sni := sharedConfigCache.Get().TLSDomain; sni != "" {
		r.FakeSNI = sni
	}
	return r
}

// RunGlobalpingCheck — медленная внешняя проверка (HEAD/HTTPS с fake-SNI из РФ-проб).
// Отчёт всегда содержит measurement_id (кроме случая, когда create упал ещё до id).
func RunGlobalpingCheck(cfg *NodeConfig, nodeID, ip string) HealthReport {
	telemtCfg, terr := loadTelemtConfig(cfg.Telemt.ConfigPath)
	report := baseReport(nodeID, ip, telemtCfg)

	metricsOK, snapshot, metricsChecked := getCachedMetrics()
	report.MetricsOK = metricsOK
	if metricsChecked {
		report.MetricsSnapshot = snapshot
	}

	if report.FakeSNI == "" {
		report.Error = "shared config not yet loaded from registry (fake SNI unknown)"
		report.Healthy = false
		return report
	}
	if terr != nil {
		report.Error = terr.Error()
		report.Healthy = false
		return report
	}
	if ip == "" {
		report.Error = "public IP not yet detected"
		report.Healthy = false
		return report
	}

	// Не отстреливаем globalping-меasurement в лежащий/рестартующий
	// прокси — пробы пойдут в закрытый порт, и верификация нарисует ratio 0
	// (ложная блокировка). Порт ещё не слушается (старт, рестарт после патча
	// конфига) — ждём до gpProxyWaitTimeout; не дождались — пропускаем цикл,
	// measurement не создаётся, и регистратор сохраняет прежний GP-статус.
	if !waitProxyTCP(report.Port, gpProxyWaitTimeout) {
		report.Error = fmt.Sprintf("proxy port %d not listening after %s — globalping skipped", report.Port, gpProxyWaitTimeout)
		report.GlobalpingOK = false
		report.Healthy = false
		return report
	}

	gp := NewGlobalpingChecker(cfg.Globalping.APIBase) // api_base из конфига
	measID, ratio, err := gp.CreateAndAwait(ip, report.Port, report.FakeSNI)
	report.GlobalpingMeasurementID = measID
	report.GlobalpingSuccessRatio = ratio
	if err != nil {
		report.Error = "globalping: " + err.Error()
		report.GlobalpingOK = false
	} else {
		report.GlobalpingOK = ratio >= 0.5
	}

	report.Healthy = report.GlobalpingOK && metricsOK
	return report
}

// RunMetricsCheck — быстрая локальная проверка telemt /metrics.
// Отчёт намеренно НЕ содержит measurement_id: регистратор при таком отчёте
// не трогает ранее верифицированный globalping-статус.
func RunMetricsCheck(cfg *NodeConfig, nodeID, ip string) HealthReport {
	telemtCfg, terr := loadTelemtConfig(cfg.Telemt.ConfigPath)
	report := baseReport(nodeID, ip, telemtCfg)

	if terr != nil {
		report.Error = terr.Error()
		report.Healthy = false
		return report
	}

	url := metricsURLFromTelemt(telemtCfg.Server.MetricsPort, telemtCfg.Server.MetricsListen)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err == nil && resp.StatusCode == http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		// Клиенты считаются только по общему секрету — в агрегат
		// входят лишь пользователи из shared-конфига регистратора.
		snapshot, values := buildMetricsSnapshot(parsePrometheusSamples(string(body)), sharedConfigCache.Get().Users)
		report.MetricsSnapshot = snapshot

		v, ok := values[healthMetricName]
		if !ok {
			report.Error = fmt.Sprintf("metrics: metric %s not found in %s output", healthMetricName, url)
			report.MetricsOK = false
		} else {
			report.MetricsOK = v > 0
		}
		cacheMetrics(report.MetricsOK, snapshot)
		report.Healthy = report.MetricsOK
		return report
	}
	if resp != nil {
		resp.Body.Close()
	}

	// /metrics недоступен — нода нездорова, без обходных путей:
	// Telemt REST API не используем (выпилен, "оно не работает").
	promErr := ""
	if err != nil {
		promErr = "metrics: fetch error: " + err.Error() + " (" + url + ")"
	} else {
		promErr = fmt.Sprintf("metrics: %s returned status %d", url, resp.StatusCode)
	}
	report.Error = promErr
	report.MetricsOK = false
	cacheMetrics(false, nil)
	return report
}

// TerminatedError — регистратор ответил kill-сигналом
// (403 {"terminate":true,...}): нода убита навсегда, обязана записать
// Message в лог дословно, положить tombstone и остановиться
// (terminate.go). Причина: ip_ban — GP-карантин исчерпан, dead — порт/
// метрики не отвечали дольше terminate_dead_min.
type TerminatedError struct {
	Reason  string
	Message string
}

func (e *TerminatedError) Error() string {
	return "terminated by registry: " + e.Reason + " (" + e.Message + ")"
}

// parseTerminateBody — разбор kill-ответа регистратора; ok=false — обычная
// ошибка (не terminate).
func parseTerminateBody(body []byte) (*TerminatedError, bool) {
	var p struct {
		Terminate bool   `json:"terminate"`
		Reason    string `json:"reason"`
		Message   string `json:"message"`
	}
	if err := json.Unmarshal(body, &p); err != nil || !p.Terminate || p.Reason == "" {
		return nil, false
	}
	return &TerminatedError{Reason: p.Reason, Message: p.Message}, true
}

func SendReport(cfg *NodeConfig, report HealthReport) error {
	data, _ := json.Marshal(report)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := registryRequest(client, cfg, http.MethodPost, "/report", bytes.NewReader(data))
	if err != nil {
		netw.noteFail() // сетевой вотчдог: сброс keep-alive / детект смены IP
		return err
	}
	netw.noteOK()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		if te, ok := parseTerminateBody(body); ok {
			return te
		}
		return fmt.Errorf("report rejected: status=%d body=%s", resp.StatusCode, string(body))
	}
	return nil
}
