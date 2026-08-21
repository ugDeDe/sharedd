package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type registerPayload struct {
	NodeID string `json:"node_id"`
	IP     string `json:"ip"`
	// NodeType: classic / mtproxyl / meko — информационный бейдж в
	// панели (см. nodetype.go).
	NodeType string `json:"node_type,omitempty"`
}

var nodeID string

// gpKick — толчок ВНЕОЧЕРЕДНОЙ globalping-проверки: каждая успешная
// регистрация (200) будит globalpingLoop немедленно, не дожидаясь таймера
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
// другой менеджер (classic ↔ mtproxyl ↔ meko) панель увидит без смены IP.
var lastNodeType string

func main() {
	for _, arg := range os.Args[1:] {
		if arg == "-version" || arg == "--version" {
			fmt.Println("sharedd-node-agent")
			return
		}
	}
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
	cfg, err := loadNodeConfig()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	// One-shot конвейер для установщика — применить конфиг и выйти
	// (стоп → патч → старт → ожидание /metrics → откат; коды выхода см.
	// apply_once.go). Демон в этом режиме не запускается.
	if applyOnceFlag() {
		os.Exit(runApplyOnce(cfg))
	}

	nodeID, err = resolveNodeID()
	if err != nil {
		log.Fatalf("failed to resolve node id: %v", err)
	}
	log.Printf("node id: %s (persistent random)", nodeID)

	ipr := newIPResolver()
	if cfg.Node.PublicIP != "" { // ручной адрес (DNAT/hairpin)
		ipr.fixed = cfg.Node.PublicIP
	}

	// Нода когда-то была терминально завершена — проверяем право на
	// Воскрешение ДО любых регистраций (ip сменился ИЛИ регистратор
	// дал gp re-verify — иначе служба останавливается, новых регистраций
	// не будет).
	client := &http.Client{Timeout: 5 * time.Second}

	// Сетевой вотчдог: после ручной смены IP/шлюза на хостинге агент сам
	// сбрасывает протухшие соединения, детектит новый адрес и в крайнем
	// случае перезапускается (netwatch.go).
	netw.bind(client, ipr)

	checkTerminationTombstone(cfg, ipr, client)

	ip, err := ipr.Current(true)
	if err != nil {
		log.Printf("initial public IP detection failed: %v (will keep retrying in heartbeat loop)", err)
	} else {
		warnIfNonPublicIP(ip)
		register(client, cfg, ip)
	}

	if shared, err := fetchSharedConfig(cfg); err == nil {
		applySharedConfig(cfg, shared)
	} else {
		log.Printf("initial shared config fetch failed: %v (will retry in background)", err)
	}

	go syncLoop(cfg)
	go antiscanLoop(cfg)
	go heartbeatLoop(client, cfg, ipr)
	go globalpingLoop(cfg, ipr)
	metricsLoop(cfg, ipr) // блокирующий, в main goroutine
}

// register — POST /register. ok=true только при 200. Если регистратор держит
// ноду в карантине после prune, регистрация отклоняется
// 429 + Retry-After — тогда ok=false и retryAfter>0: вызывающая сторона
// ОБЯЗАНА глушить повторные попытки до дедлайна (см. heartbeatLoop), иначе
// вернёмся к долбёжке, которую карантин как раз призван прекратить.
func register(client *http.Client, cfg *NodeConfig, ip string) (bool, time.Duration) {
	if ip == "" {
		return false, 0
	}
	nt := detectNodeType()
	payload := registerPayload{NodeID: nodeID, IP: ip, NodeType: nt}
	data, _ := json.Marshal(payload)
	resp, err := registryRequest(client, cfg, http.MethodPost, "/register", bytes.NewReader(data))
	if err != nil {
		log.Printf("register error: %v", err)
		netw.noteFail() // сброс keep-alive; при смене исходящего IP — кэша IP/рестарт
		return false, 0
	}
	netw.noteOK()
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	// Терминальный бан — регистрация запрещена навсегда; завершаемся
	// (сообщение регистратора уходит в лог дословно). Функция не возвращается.
	if resp.StatusCode == http.StatusForbidden {
		if te, ok := parseTerminateBody(body); ok {
			selfTerminate(cfg, te.Reason, te.Message, ip)
		}
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		// Prune-карантин на регистраторе. Регистрироваться раньше
		// дедлайна нет смысла — TTL карантина растёт с каждым strike.
		retryAfter := parseRetryAfterSeconds(resp.Header.Get("Retry-After"))
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
		// Внеочередной globalping сразу после успешной регистрации —
		// регистратор получает свежий отчёт немедленно, нода возвращается
		// в пул без ожидания таймера (после восстановления сети/смены IP).
		kickGlobalping()
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

func heartbeatLoop(client *http.Client, cfg *NodeConfig, ipr *ipResolver) {
	lastRegisteredIP := ""
	// Право молчания сведено в netGate (localhealth.go): режим тихого
	// лечения (локальные проверки красные) и prune-карантин (429 Retry-After)
	// оба означают ПОЛНОЕ молчание для регистратора — ни heartbeat, ни
	// отчётов. Молчание снимает ноду из пула за heartbeat_ttl; возврат —
	// первый же heartbeat после дедлайна/оздоровления → 410 → register.

	tryRegister := func(ip string) {
		if ip == "" || gate.silent() {
			return
		}
		ok, retryAfter := register(client, cfg, ip)
		if ok {
			lastRegisteredIP = ip
			return
		}
		if retryAfter > 0 {
			gate.noteBan(time.Now().Add(retryAfter))
		}
	}

	for {
		if gate.silent() {
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
		if ip != "" && (ip != lastRegisteredIP || detectNodeType() != lastNodeType) {
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
			resp.Body.Close()
			// Kill-сигнал — нода терминально убита, завершаемся.
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

func globalpingLoop(cfg *NodeConfig, ipr *ipResolver) {
	for {
		// Коалесценция: прогон начинается прямо сейчас — висящий толчок
		// съедаем, чтобы не сделать ВТОРОЙ прогон сразу после этого.
		select {
		case <-gpKick:
		default:
		}
		// Тихое лечение при мёртвых локальных метриках или активном
		// prune-карантине measurement'ы не создаём вовсе (жечь квоту по
		// Мёртвому прокси незачем). GP-нога НЕМОТЫ убрана — нода с
		// «всё зелёное, кроме GP» обязана продолжать отчёты: её судьбу
		// (карантин → N попыток → бан) и доставку kill-сигнала ведёт
		// регистратор (registry/terminate.go).
		if gate.metricsMuted() || gate.banActive() {
			waitGlobalpingTick()
			continue
		}
		ip, err := ipr.Current(false)
		if err != nil {
			log.Printf("globalping check skipped: no public IP: %v", err)
			ip = ""
		}
		report := RunGlobalpingCheck(cfg, nodeID, ip)
		if report.Error != "" {
			log.Printf("globalping check: %s", report.Error)
		} else {
			log.Printf("globalping check ok=%v (ratio=%.2f) measurement=%s",
				report.GlobalpingOK, report.GlobalpingSuccessRatio, report.GlobalpingMeasurementID)
		}
		if gate.silent() {
			// Между проверкой и отправкой ушли в немоту (метрики/бан) —
			// отчёт не шлём.
			waitGlobalpingTick()
			continue
		}
		if err := SendReport(cfg, report); err != nil {
			var te *TerminatedError
			if errors.As(err, &te) { // kill-сигнал при ответе на отчёт
				selfTerminate(cfg, te.Reason, te.Message, ip)
			}
			log.Printf("failed to send globalping report: %v", err)
		} else {
			log.Printf("globalping report accepted by registry: measurement=%s", report.GlobalpingMeasurementID)
		}
		waitGlobalpingTick()
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

func metricsLoop(cfg *NodeConfig, ipr *ipResolver) {
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
		// это терминальный класс dead. Агент умирает сам, успевая сообщить
		// регистратору /retire (бан — в вечную историю). В лог уходит msgDead.
		if win := cfg.deadKill(); win > 0 && gate.deadKillDue(time.Now(), win) {
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
