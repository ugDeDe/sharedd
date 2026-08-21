package main

// Терминальное завершение ноды.
//
// Регистратор (registry/terminate.go) убивает ноду навсегда в двух классах:
//
//	ip_ban — не прошла globalping (карантин исчерпал попытки или нода
//	 вышла из карантина по expiry/dead), значит IP заблокирован
//	 снаружи. Агент при этом НЕ останавливает службу: он уходит в
//	 режим ожидания смены IP (awaitIPChange) — полное молчание для
//	 регистратора + перепроверка публичного адреса раз в минуту.
//	 Оператор меняет IP на хостинге → агент замечает это сам,
//	 стирает tombstone и перезапускается (exit(1) под Restart=always),
//	 свежий процесс регистрируется с нового адреса — регистратор
//	 снимает бан регистрацией с НОВОГО ip. Ручной restart службы
//	 тоже работает, как раньше: boot-проверка со сменившимся ip
//	 стирает tombstone; с тем же ip — запрашивает GP-перепроверку,
//	 а при отказе снова ждёт смену IP (не умирает).
//	dead — регистратор не достучался до порта и/или не получил метрики
//	 дольше terminate_dead_min. Агент сам детектирует тот же факт по
//	 непрерывно красным локальным scrape'ам (netGate.mMutedSince) и
//	 умирает без всякого сигнала сверху, перед смертью успевая
//	 сообщить POST /retire — бан попадает в вечную историю регистратора.
//
// «Нода мертва, не должна делать новую регистрацию» реализовано tombstone-
// файлом (/var/lib/sharedd/terminated.json): любой (пере)запуск службы с
// валидным tombstone пишет сообщение в лог и снова останавливается —
// systemd Restart=always это переживает (мы останавливаем СВОЙ юнит
// systemctl stop, а не просто exit'имся). Снять вручную: устранить причину
// и удалить tombstone.
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
	"os/exec"
	"sync"
	"time"
)

const (
	reasonIPBan = "ip_ban"
	reasonDead  = "dead"

	msgIPBan = "Бан по ip, запустите службу заново после его смены"
	msgDead  = "Регистратор не достучался до порта и/или не получил метрики"
)

// tombstonePath — var ради тестов (прод: /var/lib/sharedd/terminated.json).
var tombstonePath = "/var/lib/sharedd/terminated.json"

// agentUnitName — наш systemd-юнит (scripts/install_node.sh).
var agentUnitName = "sharedd-node-agent.service"

// Таймауты boot-перевалидации — var ради тестов.
var (
	bootIPWait       = 30 * time.Second // сколько ждём определения публичного IP на старте (ip_ban)
	bootPollInterval = 5 * time.Second
)

// ipBanPollInterval — период перепроверки публичного IP в режиме ожидания
// смены адреса после ip_ban (var ради тестов).
var ipBanPollInterval = time.Minute

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

// ipBanRecover — рантайм-обработка ip_ban (kill-сигнал 403 посреди работы).
// Раньше здесь была остановка службы с надписью «запустите службу заново
// после его смены» — после смены IP нода сама НЕ возвращалась, прокси
// простаивал, пока оператор не вспомнит про systemctl restart. Теперь:
// ждём смену публичного адреса (перепроверка раз в минуту), затем стираем
// tombstone и перезапускаемся через systemd (exit(1) при Restart=always) —
// свежий процесс регистрируется с нового IP, регистратор снимает бан
// регистрацией с нового адреса. В проде Do не возвращается (exit), вторые
// и последующие горутины блокируются в Do — молчание для регистратора
// обеспечивается самой блокировкой.
func ipBanRecover(bannedIP string) {
	ipBanOnce.Do(func() {
		ipr := netw.resolver()
		if ipr == nil { // ранний вызов до bind (теоретический)
			ipr = newIPResolver()
		}
		newIP := awaitIPChangeFn(bannedIP, ipr)
		clearTombstone()
		if systemdAvailable() && unitLoaded(agentUnitName) {
			log.Printf("restarting agent to re-register from the new IP %s (systemd will bring it back up)", newIP)
		} else {
			log.Printf("no systemd unit — exiting so the supervisor/operator restarts the agent (new IP %s, tombstone cleared)", newIP)
		}
		exitProcess(1) // Restart=always поднимет процесс с чистого листа
	})
}

// exitProcess — подменяется в тестах, dieSelf должен «завершать» процесс.
var exitProcess = os.Exit

type termination struct {
	Reason string    `json:"reason"`
	IP     string    `json:"ip"`
	At     time.Time `json:"at"`
}

func readTombstone() *termination {
	data, err := os.ReadFile(tombstonePath)
	if err != nil {
		return nil
	}
	var t termination
	if err := json.Unmarshal(data, &t); err != nil || t.Reason == "" {
		return nil
	}
	return &t
}

func writeTombstone(t termination) {
	t.At = time.Now()
	data, _ := json.MarshalIndent(t, "", " ")
	f, err := os.OpenFile(tombstonePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err == nil {
		err = f.Chmod(0600)
	}
	if err == nil {
		_, err = f.Write(data)
	}
	if f != nil {
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		log.Printf("failed to write termination tombstone %s: %v", tombstonePath, err)
	}
}

func clearTombstone() {
	if err := os.Remove(tombstonePath); err != nil && !os.IsNotExist(err) {
		log.Printf("failed to remove termination tombstone %s: %v", tombstonePath, err)
	}
}

// dieSelf — финал: сообщение в лог (дословно), останов СВОЕГО systemd-юнита
// (service stays dead даже при Restart=always — это осознанный стоп, не
// падение), без systemd — просто выход. Функция НЕ возвращается.
// dieSelfFn — var-обёртка ради тестов (они подменяют «завершение»).
var dieSelfFn = dieSelf

func dieSelf(message string) {
	log.Print(message)
	log.Printf("node terminated — new registration is forbidden (tombstone: %s; снимается удалением файла после устранения причины)", tombstonePath)
	if systemdAvailable() && unitLoaded(agentUnitName) {
		if err := exec.Command("systemctl", "stop", agentUnitName).Run(); err != nil {
			log.Printf("systemctl stop %s failed: %v — exiting anyway", agentUnitName, err)
		}
	}
	exitProcess(0)
	select {} // exitProcess подменён в тестах — дальше не идём
}

// selfTerminate — kill-сигнал от регистратора (403 terminate на любом нашем
// запросе) ИЛИ локальный вердикт dead. Для локального dead сначала шлём
// POST /retire (best-effort, короткий таймаут — умирать всё равно).
func selfTerminate(cfg *NodeConfig, reason, message, ip string) {
	if message == "" {
		message = msgIPBan
		if reason == reasonDead {
			message = msgDead
		}
	}
	writeTombstone(termination{Reason: reason, IP: ip})
	if reason == reasonDead && cfg != nil && ip != "" {
		body, _ := json.Marshal(map[string]string{"node_id": nodeID, "ip": ip, "reason": reasonDead})
		client := &http.Client{Timeout: 4 * time.Second}
		resp, err := registryRequest(client, cfg, http.MethodPost, "/retire", bytes.NewReader(body))
		if err != nil {
			log.Printf("retire notice to registry failed: %v (ban history may miss this node)", err)
		} else {
			resp.Body.Close()
		}
	}
	// ip_ban НЕ терминален для службы: агент остаётся жить и сам ждёт
	// смену IP (перепроверка раз в минуту), после смены — рестарт и
	// регистрация с нового адреса. Горутина-вызвавший блокируется здесь
	// навсегда (как раньше блокировалась в dieSelf) — молчание для
	// регистратора обеспечено.
	if reason == reasonIPBan && ip != "" {
		ipBanRecover(ip)
		return // только тесты: боевой ipBanRecover не возвращается
	}
	dieSelfFn(message)
}

// checkTerminationTombstone — старт демона с tombstone'ом: решаем, ожила ли
// нода (ip_ban: сменился ip, либо регистратор дал GP-перепроверку; dead —
// никогда), иначе служба немедленно останавливается обратно. Зелёный исход —
// tombstone стирается, демон идёт дальше как обычно.
func checkTerminationTombstone(cfg *NodeConfig, ipr *ipResolver, client *http.Client) {
	t := readTombstone()
	if t == nil {
		return
	}
	switch t.Reason {
	case reasonIPBan:
		// Путь 1 — смена IP: сверяем текущий публичный адрес с забаненным;
		// изменился — tombstone стирается (регистратор снимет блок
		// регистрацией с нового ip).
		log.Printf("termination tombstone present (ip_ban of %s since %s) — checking public IP", t.IP, t.At.Format(time.RFC3339))
		deadline := time.Now().Add(bootIPWait)
		var ip string
		for {
			cur, err := ipr.Current(true)
			if err == nil && isPublicIP(cur) {
				ip = cur
				break
			}
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(bootPollInterval)
		}
		if ip != "" && ip != t.IP {
			log.Printf("public IP changed %s -> %s — ban lifted, resuming service", t.IP, ip)
			clearTombstone()
			return
		}
		// Путь 2: ip тот же — просим у регистратора GP-перепроверку
		// вместо мгновенной смерти. Регистратор + вернёт 200/429
		// (карантин с одной решающей попыткой); kill (403) внутри register
		// завершит нас с tombstone'ом (для ip_ban selfTerminate уйдёт в
		// ожидание смены IP, не в стоп службы).
		if ip != "" && cfg != nil {
			log.Printf("public IP unchanged (%s) — requesting gp re-verify from registry", ip)
			ok, retryAfter := register(client, cfg, ip)
			if ok || retryAfter > 0 {
				log.Printf("registry opened re-verify (ok=%t, retry_after=%s) — clearing tombstone, resuming service",
					ok, retryAfter.Round(time.Second))
				clearTombstone() // повторный kill, если придёт, положит новый
				return
			}
			log.Printf("re-verify request failed (registry unreachable?) — waiting for an IP change instead")
		}
		// Служба НЕ останавливается: ждём смену IP и возвращаемся сами
		// (раньше здесь был dieSelf — нода лежала до ручного systemctl
		// restart даже после того, как оператор сменил адрес).
		ipBanRecover(t.IP)
	default:
		// dead: нода мертва навсегда, повторной регистрации не делать ни в
		// каком случае — даже починившийся прокси её не воскрешает (регистратор
		// всё равно откажет). Воскрешение — только осознанное ручное:
		// удалить tombstone + попросить снять запись на регистраторе.
		log.Printf("termination tombstone present (dead since %s) — staying dead", t.At.Format(time.RFC3339))
		dieSelfFn(msgDead)
	}
}

// isPublicIP — минимальная проверка «не приватный/не пустой» для boot-сверки
// tombstone. Полная валидация остаётся на warnIfNonPublicIP при регистрации.
func isPublicIP(ip string) bool {
	if ip == "" {
		return false
	}
	p := net.ParseIP(ip)
	return p != nil && !p.IsPrivate() && !p.IsLoopback() && !p.IsUnspecified()
}
