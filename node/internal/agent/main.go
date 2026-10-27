package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"sharedd/node/internal/config"
)

type registerPayload struct {
	NodeID string `json:"node_id"`
	IP     string `json:"ip"`
	// NodeType: classic / mtproxyl / meko / mtdd — информационный бейдж в
	// панели (см. nodetype.go).
	NodeType string `json:"node_type,omitempty"`
	// AuthKey — персональный ключ регистрации из [registry] authkey
	// (invitations-migration-contract.md §1): при выключенном общем
	// секрете реестр принимает /register по нему.
	AuthKey string `json:"authkey,omitempty"`
}

var nodeID string

// Персональный секрет ноды (invitations-migration-contract.md §1):
// реестр выдаёт его в ответе каждого успешного /register; агент хранит
// его рядом с node_id (0600) и отправляет как Bearer на всех маршрутах.
// Пусто = старый реестр без секретов / ещё не зарегистрировались — общий
// токен из конфига остаётся транспортом (совместимость).
const defaultNodeSecretFile = "/var/lib/sharedd/node_secret"

var nodeSecretFile = defaultNodeSecretFile // var ради тестов
var nodeSecret string
var secretMu sync.RWMutex

func currentNodeSecret() string {
	secretMu.RLock()
	defer secretMu.RUnlock()
	return nodeSecret
}

// loadNodeSecret — секрет из файла рядом с node_id, иначе из [registry]
// node_secret (ручное предвыданное значение). Вызывается один раз при старте.
func loadNodeSecret(cfg *config.NodeConfig) {
	if b, err := os.ReadFile(nodeSecretFile); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			nodeSecret = s
			return
		}
	}
	nodeSecret = strings.TrimSpace(cfg.Registry.NodeSecret)
}

// persistNodeSecret — сохранить выданный секрет (0600) и сразу использовать.
// Неудача записи не роняет агента: секрет живёт в памяти до рестарта.
func persistNodeSecret(secret string) {
	secretMu.Lock()
	defer secretMu.Unlock()
	if err := os.WriteFile(nodeSecretFile, []byte(secret+"\n"), 0600); err != nil {
		log.Printf("failed to persist node secret to %s: %v (kept in memory)", nodeSecretFile, err)
	}
	nodeSecret = secret
}

// clearNodeSecret — реестр отклонил наш секрет (401): забываем его, чтобы
// последующие попытки шли с общим токеном/authkey (перевыпуск на /register).
func clearNodeSecret() {
	secretMu.Lock()
	defer secretMu.Unlock()
	nodeSecret = ""
	if err := os.Remove(nodeSecretFile); err != nil && !os.IsNotExist(err) {
		log.Printf("failed to remove stale node secret: %v", err)
	}
}

// gpKick — толчок ВНЕОЧЕРЕДНОЙ globalping-проверки: обычная успешная
// регистрация (200) будит globalpingLoop; recovery из GP не будит его обратно.
// Обычная регистрация не дожидается таймера
// (ум. 5 мин). Иначе после восстановления сети/смены IP нода уже
// перерегистрировалась, но регистратор до следующего тика не видит живого
// globalping-отчёта — прокси зря простаивает вне пула. Буфер 1: слившиеся
// подряд регистрации дают один внеочередной прогон (квота GP не горит).
var gpKick = make(chan struct{}, 1)

func kickGlobalping() {
	select {
	case gpKick <- struct{}{}:
	default: // толчок уже в очереди — второй не нужен
	}
}

// lastNodeType — тип менеджера, с которым последний раз УСПЕШНО (HTTP 200)
// регистрировались; heartbeat сверяет текущий с ним — переустановку ноды на
// другой менеджер (classic ↔ mtproxyl ↔ meko ↔ mtdd) панель увидит без смены IP.
var lastNodeType string
var registrationMu sync.Mutex

func registeredNodeType() string {
	registrationMu.Lock()
	defer registrationMu.Unlock()
	return lastNodeType
}

func Run() {
	for i, arg := range os.Args[1:] {
		path := ""
		if strings.HasPrefix(arg, "--telemt-port=") {
			path = strings.TrimPrefix(arg, "--telemt-port=")
		} else if arg == "--telemt-port" && i+2 < len(os.Args) {
			path = os.Args[i+2]
		}
		if path != "" {
			cfg, err := loadTelemtConfig(path)
			if err != nil {
				log.Fatal(err)
			}
			fmt.Println(telemtProxyPort(cfg))
			return
		}
	}
	cfg, err := config.LoadNodeConfig(nodeConfigPathFlag())
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	// One-shot конвейер для установщика — применить конфиг и выйти
	// (стоп → патч → старт → ожидание /metrics → откат; коды выхода см.
	// apply_once.go). Демон в этом режиме не запускается.
	if applyOnceFlag() {
		os.Exit(runApplyOnce(cfg))
	}

	nodeID, err = config.ResolveID(defaultIDStateFile)
	if err != nil {
		log.Fatalf("failed to resolve node id: %v", err)
	}
	log.Printf("node id: %s (persistent random)", nodeID)
	loadNodeSecret(cfg)
	if nodeSecret != "" {
		log.Printf("node secret loaded (personal bearer authentication active)")
	}

	ipr := newIPResolver()
	if cfg.Node.PublicIP != "" { // ручной адрес (DNAT/hairpin)
		ipr.fixed = cfg.Node.PublicIP
	}

	// Блокировки ноды живёт только на регистраторе: нода при любом старте
	// регистрируется как обычно, решение (принять / reverify-карантин /
	// 429 / kill) принимает регистратор.
	client := &http.Client{Timeout: 5 * time.Second}

	// Сетевой вотчдог: после ручной смены IP/шлюза на хостинге агент сам
	// сбрасывает протухшие соединения, детектит новый адрес и в крайнем
	// случае перезапускается (netwatch.go).
	netw.bind(client, ipr)
	// GP must start before any registry request, including rejected initial
	// registration or an unavailable shared-config service.
	go globalpingLoop(cfg, ipr)

	ip, err := ipr.Current(true)
	if err != nil {
		log.Printf("initial public IP detection failed: %v (will keep retrying in heartbeat loop)", err)
	} else {
		warnIfNonPublicIP(ip)
		tryRegistration(client, cfg, ip, false)
	}

	if shared, err := fetchSharedConfig(cfg); err == nil {
		applySharedConfig(cfg, shared)
	} else {
		log.Printf("initial shared config fetch failed: %v (will retry in background)", err)
	}

	go syncLoop(cfg)
	go antiscanLoop(cfg)
	go heartbeatLoop(client, cfg, ipr)
	metricsLoop(cfg, ipr) // блокирующий, в main goroutine
}

// register — POST /register. ok=true только при 200. Если регистратор держит
// ноду в карантине после prune, регистрация отклоняется
// 429 + Retry-After — тогда ok=false и retryAfter>0; общий путь обновляет
// prune silence, а runtime tryRegistration соблюдает единый backoff gate.
func register(client *http.Client, cfg *config.NodeConfig, ip string) (bool, time.Duration) {
	return registerWithReason(client, cfg, ip, false, false)
}

func registerWithReason(client *http.Client, cfg *config.NodeConfig, ip string, fromGP, bootstrap bool) (bool, time.Duration) {
	registrationMu.Lock()
	defer registrationMu.Unlock()
	if ip == "" || cfg == nil {
		return false, 0
	}
	nt := detectNodeType()
	payload := registerPayload{NodeID: nodeID, IP: ip, NodeType: nt, AuthKey: cfg.Registry.AuthKey}
	data, _ := json.Marshal(payload)
	resp, err := registryRequest(client, cfg, http.MethodPost, "/register", bytes.NewReader(data))
	if err != nil {
		log.Printf("register error: %v", err)
		netw.noteFail() // сброс keep-alive; при смене исходящего IP — кэша IP/рестарт
		return false, 0
	}
	netw.noteOK()
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	// 401 — реестр отклонил наши учётные данные. Если это был персональный
	// секрет (перевыпущен/отозван), забываем его: следующая попытка пойдёт
	// с общим токеном (ON) или authkey в теле (OFF), /register выдаст новый.
	// Исключение: bootstrap/GP recovery сохраняют те же credentials на retry.
	if resp.StatusCode == http.StatusUnauthorized && !fromGP && !bootstrap && currentNodeSecret() != "" {
		log.Printf("registry rejected the stored node secret — clearing it for re-issue")
		clearNodeSecret()
	}
	// Health refusals arm nonblocking recovery; credentials and IP survive.
	if resp.StatusCode == http.StatusForbidden {
		if te, ok := parseTerminateBody(body); ok {
			selfTerminate(cfg, te.Reason, te.Message, ip)
		}
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		// Prune-карантин на регистраторе. Регистрироваться раньше
		// дедлайна нет смысла — TTL карантина растёт с каждым strike.
		retryAfter := parseRetryAfterSeconds(resp.Header.Get("Retry-After"))
		// All callers (startup, bootstrap, GP and heartbeat) must observe the
		// authoritative prune silence. Ordinary network backoff is not a ban.
		gate.noteBan(time.Now().Add(retryAfter))
		log.Printf("register deferred by registry: node was pruned as inactive, retry after %s",
			retryAfter.Round(time.Second))
		return false, retryAfter
	}
	if nt != lastNodeType && lastNodeType != "" {
		log.Printf("node type changed: %s -> %s", nodeTypeLabel(lastNodeType), nodeTypeLabel(nt))
	}
	log.Printf("registered as %s (%s) type=%s, status=%d", nodeID, ip, nodeTypeLabel(nt), resp.StatusCode)
	if resp.StatusCode == http.StatusOK {
		lastNodeType = nt
		// Выданный персональный секрет (contract §1): тело может его не
		// содержать (старый реестр) — молча продолжаем с общим токеном.
		var regResp struct {
			NodeSecret string `json:"node_secret"`
		}
		if json.Unmarshal(body, &regResp) == nil && regResp.NodeSecret != "" && regResp.NodeSecret != currentNodeSecret() {
			persistNodeSecret(regResp.NodeSecret)
			log.Printf("node secret issued by registry — stored, personal bearer authentication active")
		}
		// Внеочередной globalping сразу после успешной регистрации —
		// регистратор получает свежий отчёт немедленно, нода возвращается
		// в пул без ожидания таймера (после восстановления сети/смены IP).
		if !fromGP {
			kickGlobalping()
		}
	}
	return resp.StatusCode == http.StatusOK, 0
}

// parseRetryAfterSeconds — Retry-After в формате delta-seconds (HTTP-дату
// регистратор не шлёт; на ней — fail-open 0, попробуем на следующем тике).
func parseRetryAfterSeconds(h string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(h))
	if err != nil || n < 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

func heartbeatLoop(client *http.Client, cfg *config.NodeConfig, ipr *ipResolver) {
	// Право молчания сведено в netGate (localhealth.go): режим тихого
	// лечения (локальные проверки красные) и prune-карантин (429 Retry-After)
	// оба останавливают heartbeat и metrics reports, но НЕ локальный GP.
	// GP recovery имеет отдельный bounded register/report путь.
	// Молчание снимает ноду из пула за heartbeat_ttl; возврат —
	// первый же heartbeat после дедлайна/оздоровления → 410 → register.

	tryRegister := func(ip string) {
		if ip == "" || gate.silent() {
			return
		}
		tryRegistration(client, cfg, ip, false)
	}

	for {
		if gate.silent() || recovery.needed() {
			// Bootstrap may need /register to obtain any shared config at all.
			// After the first success, health recovery still requires completed GP.
			retryBootstrapRegistration(client, cfg, ipr)
			time.Sleep(intervals.Heartbeat())
			continue
		}

		// IP может поменяться (динамический/перевыделенный) — тогда
		// пере-регистрируемся, т.к. heartbeat-ручка IP не обновляет.
		ip, err := ipr.Current(false)
		if err != nil {
			log.Printf("IP detection failed: %v", err)
			ip = ""
		}

		// Перерегистрация нужна при смене публичного IP и при смене типа
		// менеджера (переустановили ноду на другой форк — бейдж типа в панели
		// должен обновиться; register сам обновит lastNodeType при 200).
		recovery.mu.Lock()
		lastRegisteredIP := recovery.registeredIP
		recovery.mu.Unlock()
		if ip != "" && (ip != lastRegisteredIP || detectNodeType() != registeredNodeType()) {
			tryRegister(ip)
			time.Sleep(intervals.Heartbeat())
			continue
		}

		payload := map[string]string{"node_id": nodeID}
		data, _ := json.Marshal(payload)
		resp, err := registryRequest(client, cfg, http.MethodPost, "/heartbeat", bytes.NewReader(data))
		if err != nil {
			log.Printf("heartbeat error: %v, re-registering", err)
			// Сетевой сбой: сбросить протухшие keep-alive и перечитать
			// исходящий IP ДО повторной регистрации — после ручной смены
			// IP/шлюза register уйдёт свежим сокетом с новым адресом.
			netw.noteFail()
			if fresh, ferr := ipr.Current(false); ferr == nil && fresh != "" {
				ip = fresh
			}
			tryRegister(ip)
		} else {
			netw.noteOK()
			hbBody, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			// Health rejection schedules recovery without blocking this loop.
			if resp.StatusCode == http.StatusForbidden {
				if te, ok := parseTerminateBody(hbBody); ok {
					selfTerminate(cfg, te.Reason, te.Message, ip)
				}
			}
			if resp.StatusCode != http.StatusOK {
				log.Printf("heartbeat rejected (status=%d), re-registering", resp.StatusCode)
				tryRegister(ip)
			}
		}
		time.Sleep(intervals.Heartbeat())
	}
}

func globalpingLoop(cfg *config.NodeConfig, ipr *ipResolver) {
	for {
		// Коалесценция: прогон начинается прямо сейчас — висящий толчок
		// съедаем, чтобы не сделать ВТОРОЙ прогон сразу после этого.
		select {
		case <-gpKick:
		default:
		}
		globalpingIteration(cfg, ipr)
		waitGlobalpingTick()
	}
}

var runGlobalpingCheck = RunGlobalpingCheck

// One finite scheduler iteration, independent of health/registration gates.
func globalpingIteration(cfg *config.NodeConfig, ipr *ipResolver) {
	sendPendingRetire(cfg)
	ip, err := ipr.Current(false)
	if err != nil {
		log.Printf("globalping: public IP unavailable: %v", err)
		ip = ""
	}
	report := runGlobalpingCheck(cfg, nodeID, ip)
	if report.Error != "" {
		log.Printf("globalping check: %s", report.Error)
		return
	}
	log.Printf("globalping completed ok=%t ratio=%.2f measurement=%s", report.GlobalpingOK, report.GlobalpingSuccessRatio, report.GlobalpingMeasurementID)
	if recovery.needed() {
		if !report.GlobalpingOK || report.GlobalpingMeasurementID == "" || gate.banActive() {
			return
		}
		recovery.mu.Lock()
		if recovery.lastMeasurement == report.GlobalpingMeasurementID {
			recovery.mu.Unlock()
			return
		}
		recovery.lastMeasurement = report.GlobalpingMeasurementID
		recovery.mu.Unlock()
		// Existing registry reverify path: register opens quarantine, NOT
		// admission. /report independently GET-verifies this measurement.
		if ok, _ := tryRegistration(&http.Client{Timeout: 5 * time.Second}, cfg, ip, true); !ok {
			return
		}
	}
	if gate.banActive() {
		return
	}
	if err := SendReport(cfg, report); err != nil {
		var te *TerminatedError
		if errors.As(err, &te) {
			selfTerminate(cfg, te.Reason, te.Message, ip)
		}
		log.Printf("failed to send globalping report: %v", err)
	}
}

// waitGlobalpingTick — пауза между globalping-прогонами: обычный таймер ИЛИ
// толчок от успешной регистрации (kickGlobalping) — что случится раньше.
// Толчок будит цикл немедленно: свежезарегистрированная нода отчитывается
// сразу, а не простаивает до конца интервала (ум. 5 мин).
func waitGlobalpingTick() {
	t := time.NewTimer(intervals.Globalping())
	defer t.Stop()
	select {
	case <-t.C:
	case <-gpKick:
		log.Printf("globalping check triggered by registration (off-timer)")
	}
}

func metricsLoop(cfg *config.NodeConfig, ipr *ipResolver) {
	for {
		ip, _ := ipr.Current(false) // кэш; не критично, если пусто
		report := RunMetricsCheck(cfg, nodeID, ip)
		if report.Error != "" {
			log.Printf("metrics check: %s", report.Error)
		} else {
			log.Printf("metrics check ok=%v (%s=%v)", report.MetricsOK, healthMetricName,
				report.MetricsSnapshot[healthMetricName])
		}
		// Локальная проверка питает защёлку молчания. В режиме тихого
		// лечения (и в prune-карантине) отчёт НЕ отправляем: регистратору о
		// больной ноде знать незачем — пусть снимает её по heartbeat-TTL.
		gate.noteLocal(report.Error == "" && report.MetricsOK)
		// Метрик-немота непрерывно дольше dead_kill (ум. 10 мин) —
		// класс dead. Агент сообщает регистратору /retire (бан — в вечную
		// историю); локальные циклы продолжаются без рестарта процесса.
		if win := cfg.DeadKill(); win > 0 && gate.deadKillDue(time.Now(), win) {
			selfTerminate(cfg, reasonDead, msgDead, ip)
		}
		if gate.silent() {
			time.Sleep(intervals.Metrics())
			continue
		}
		if err := SendReport(cfg, report); err != nil {
			var te *TerminatedError
			if errors.As(err, &te) { // kill-сигнал при ответе на отчёт
				selfTerminate(cfg, te.Reason, te.Message, ip)
			}
			log.Printf("failed to send metrics report: %v", err)
		}
		time.Sleep(intervals.Metrics())
	}
}

const (
	defaultIDStateFile        = "/var/lib/sharedd/node_id"
	healthMetricName          = "telemt_me_writers_active_current"
	uniqueIPsMetricName       = "telemt_user_unique_ips_current"
	userConnsMetricName       = "telemt_user_connections_current"
	userOctetsFromMetricName  = "telemt_user_octets_from_client"
	userOctetsToMetricName    = "telemt_user_octets_to_client"
	trafficIngressMetricName  = "sharedd_traffic_ingress_bytes_total"
	trafficEgressMetricName   = "sharedd_traffic_egress_bytes_total"
	userIngressMetricName     = "sharedd_user_ingress_bytes_total"
	userEgressMetricName      = "sharedd_user_egress_bytes_total"
	trafficUsersMetricName    = "sharedd_traffic_users_fingerprint"
	hostCPUMetricName         = "sharedd_host_cpu_percent"
	hostRAMMetricName         = "sharedd_host_ram_percent"
	metricsListenKey          = "metrics_listen"
	metricsListenValue        = "127.0.0.1:9090"
	metricsPortKey            = "metrics_port"
	defaultIntervalsHeartbeat = 15000
	defaultIntervalsGlobal    = 300000
	defaultIntervalsMetrics   = 60000
	defaultIntervalsSync      = 60000
)

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
