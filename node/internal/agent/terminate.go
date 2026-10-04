package agent

// Завершение ноды и самостоятельное возвращение в строй.
//
// Регистратор (registry/internal/server/terminate.go) ставит ноде
// терминальную запись в двух классах:
//
//	ip_ban — не прошла globalping (карантин исчерпал попытки или нода
//	 вышла из карантина по expiry/dead), значит IP заблокирован
//	 снаружи. Агент при этом НЕ останавливает службу: он уходит в
//	 режим ожидания смены IP (awaitIPChange) — полное молчание для
//	 регистратора + перепроверка публичного адреса раз в минуту.
//	 Оператор меняет IP на хостинге → агент замечает это сам и
//	 перезапускается (exit(1) под Restart=always), свежий процесс
//	 регистрируется с нового адреса — регистратор снимает бан
//	 регистрацией с НОВОГО ip. Ручной restart службы тоже работает.
//	dead — TCP-порт не отвечает и/или метрики не идут дольше
//	 terminate_dead_min. Агент сам детектирует тот же факт по
//	 непрерывно красным локальным scrape'ам (netGate.mMutedSince),
//	 сообщает POST /retire (бан — в вечную историю регистратора) и
//	 ЖДЁТ локального восстановления, самостоятельно перепроверяя
//	 метрики. Оздоровилось — exit(1), свежий процесс перерегистрируется:
//	 регистратор снимает dead-запись первым же /register и проверяет
//	 здоровье заново. Циклов «умерла → восстановилась → вернулась»
//	 неограниченное число, tombstone-файла больше нет — единственный
//	 источник правды о блокировках теперь регистратор.
//
// Тексты сообщений — дословно из ТЗ, совпадают с MsgIPBan/MsgDead
// регистратора; регистратор присылает их же в kill-ответе (403 terminate),
// агент пишет в лог присланную строку как есть.

import (
	"bytes"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"sharedd/node/internal/config"
)

const (
	reasonIPBan = "ip_ban"
	reasonDead  = "dead"

	msgIPBan = "Бан по ip, запустите службу заново после его смены"
	msgDead  = "Регистратор не достучался до порта и/или не получил метрики"
)

// agentUnitName — наш systemd-юнит (scripts/install_node.sh).
var agentUnitName = "sharedd-node-agent.service"

// ipBanPollInterval — период перепроверки публичного IP в режиме ожидания
// смены адреса после ip_ban (var ради тестов).
var ipBanPollInterval = time.Minute

// deadRecoveryPoll — период самопроверки метрик в режиме ожидания
// восстановления после dead (var ради тестов).
var deadRecoveryPoll = 30 * time.Second

// awaitIPChangeFn — var ради тестов (боевое ожидание бесконечно).
var awaitIPChangeFn = awaitIPChange

// awaitIPChange — режим ожидания смены IP после ip_ban: блокируется, пока
// публичный адрес не станет отличным от забаненного (перепроверка раз в
// ipBanPollInterval, форс мимо кэша). Служба ПРОДОЛЖАЕТ работать — оператору
// не нужно ничего перезапускать руками; для регистратора нода в это время
// полностью молчит. Возвращает новый адрес.
func awaitIPChange(bannedIP string, ipr *ipResolver) string {
	log.Print(msgIPBan)
	log.Printf("ip_ban: жду смену публичного IP (забанен %s, перепроверка каждые %s); после смены адреса вернусь в пул сам",
		bannedIP, ipBanPollInterval)
	for {
		time.Sleep(ipBanPollInterval)
		cur, err := ipr.Current(true)
		if err != nil || !isPublicIP(cur) {
			continue
		}
		if cur != bannedIP {
			log.Printf("public IP changed %s -> %s — ban lifted, resuming service", bannedIP, cur)
			return cur
		}
	}
}

// ipBanOnce — рантайм-восстановление после ip_ban ведёт ровно ОДНА горутина;
// остальные вызвавшие selfTerminate (heartbeat/globalping/metrics могут
// словить 403 параллельно) блокируются в Do и больше регистратор не трогают.
// Pointer — тесты подменяют на свежий.
var ipBanOnce = new(sync.Once)

// ipBanRecover — рантайм-обработка ip_ban (kill-сигнал 403 посреди работы):
// ждём смену публичного адреса (перепроверка раз в минуту), затем
// перезапускаемся через systemd (exit(1) при Restart=always) — свежий
// процесс регистрируется с нового IP, регистратор снимает бан регистрацией
// с нового адреса. В проде Do не возвращается (exit), вторые и последующие
// горутины блокируются в Do — молчание для регистратора обеспечивается
// самой блокировкой.
func ipBanRecover(bannedIP string) {
	ipBanOnce.Do(func() {
		ipr := netw.resolver()
		if ipr == nil { // ранний вызов до bind (теоретический)
			ipr = newIPResolver()
		}
		newIP := awaitIPChangeFn(bannedIP, ipr)
		if systemdAvailable() && unitLoaded(agentUnitName) {
			log.Printf("restarting agent to re-register from the new IP %s (systemd will bring it back up)", newIP)
		} else {
			log.Printf("no systemd unit — exiting so the supervisor/operator restarts the agent (new IP %s)", newIP)
		}
		exitProcess(1) // Restart=always поднимет процесс с чистого листа
	})
}

// deadOnce — аналогично ipBanOnce: ожидание восстановления после dead
// ведёт ровно одна горутина, параллельные вызвавшие блокируются в Do.
// Pointer — тесты подменяют на свежий.
var deadOnce = new(sync.Once)

// awaitLocalRecoveryFn — var ради тестов (боевое ожидание бесконечно, пока
// локальные проверки красные).
var awaitLocalRecoveryFn = awaitLocalRecovery

// awaitLocalRecovery — режим ожидания после dead: основные циклы стоят
// (main-горутина заблокирована в selfTerminate), поэтому scrape'ы делаем
// САМИ, раз в deadRecoveryPoll. Зелёные метрики = прокси ожил → exit(1),
// Restart=always поднимет свежий процесс, который перерегистрируется
// (регистратор снимает dead-запись первым же /register). Не возвращается.
func awaitLocalRecovery(cfg *config.NodeConfig) {
	log.Print(msgDead)
	log.Printf("dead: жду восстановления локальных проверок (перепроверка каждые %s); после оздоровления перезапущусь и зарегистрируюсь заново",
		deadRecoveryPoll)
	for {
		time.Sleep(deadRecoveryPoll)
		if cfg == nil {
			continue
		}
		rep := RunMetricsCheck(cfg, nodeID, "")
		if rep.Error == "" && rep.MetricsOK {
			log.Print("local checks recovered — restarting agent to re-register")
			return
		}
	}
}

// deadRecover — рантайм-обработка dead: ждём локального оздоровления,
// затем перезапуск через systemd. В проде Do не возвращается (exit).
func deadRecover(cfg *config.NodeConfig) {
	deadOnce.Do(func() {
		awaitLocalRecoveryFn(cfg)
		if systemdAvailable() && unitLoaded(agentUnitName) {
			log.Print("restarting agent to re-register after recovery (systemd will bring it back up)")
		} else {
			log.Print("no systemd unit — exiting so the supervisor/operator restarts the agent")
		}
		exitProcess(1) // Restart=always поднимет процесс с чистого листа
	})
}

// exitProcess — подменяется в тестах, recovery-режимы должны «завершать» процесс.
var exitProcess = os.Exit

// selfTerminate — kill-сигнал от регистратора (403 terminate на любом нашем
// запросе) ИЛИ локальный вердикт dead. Для локального dead сначала шлём
// POST /retire (best-effort, короткий таймаут). Оба класса не останавливают
// службу: агент сам ждёт восстановления (смены IP / оздоровления метрик),
// после чего перезапускается и регистрируется заново.
func selfTerminate(cfg *config.NodeConfig, reason, message, ip string) {
	if message == "" {
		message = msgIPBan
		if reason == reasonDead {
			message = msgDead
		}
	}
	log.Print(message)
	if reason == reasonDead && cfg != nil && ip != "" {
		body, _ := json.Marshal(map[string]string{"node_id": nodeID, "ip": ip, "reason": reasonDead})
		client := &http.Client{Timeout: 4 * time.Second}
		resp, err := registryRequest(client, cfg, http.MethodPost, "/retire", bytes.NewReader(body))
		if err != nil {
			log.Printf("retire notice to registry failed: %v (ban history may miss this node)", err)
		} else {
			_ = resp.Body.Close()
		}
	}
	// Горутина-вызвавший блокируется в recovery-режиме навсегда до
	// восстановления — молчание для регистратора обеспечено.
	if reason == reasonIPBan && ip != "" {
		ipBanRecover(ip)
		return // только тесты: боевой ipBanRecover не возвращается
	}
	if reason == reasonDead {
		deadRecover(cfg)
		return // только тесты: боевой deadRecover не возвращается
	}
	// unknown reason — не блокируем цикл, просто выходим под Restart=always
	exitProcess(1)
}

// isPublicIP — минимальная проверка «не приватный/не пустой» для
// awaitIPChange. Полная валидация остаётся на warnIfNonPublicIP при
// регистрации.
func isPublicIP(ip string) bool {
	if ip == "" {
		return false
	}
	p := net.ParseIP(ip)
	return p != nil && !p.IsPrivate() && !p.IsLoopback() && !p.IsUnspecified()
}
