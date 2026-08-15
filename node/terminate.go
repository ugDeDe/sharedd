package main

// Терминальное завершение ноды.
//
// Регистратор (registry/terminate.go) убивает ноду навсегда в двух классах:
//
//	ip_ban — не прошла globalping (карантин исчерпал попытки или нода
//	 вышла из карантина по expiry/dead), значит IP заблокирован
//	 снаружи. Путей назад ДВА: перезапуск службы ПОСЛЕ
//	 СМЕНЫ ip (tombstone стирается на старте), либо перезапуск С
//	 ТЕМ ЖЕ ip — тогда запрашиваем у регистратора GP-перепроверку:
//	 регистратор + возвращает такую ноду в карантин с одной
//	 решающей попыткой (старики-регистраторы ответят 403-киллом —
//	 умрём как раньше). Защита от ложных глобалпингов и отладки
//	 «прокси был выключен».
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
	if err := os.WriteFile(tombstonePath, data, 0644); err != nil {
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
		resp, err := client.Post(cfg.Registry.URL+"/retire", "application/json", bytes.NewReader(body))
		if err != nil {
			log.Printf("retire notice to registry failed: %v (ban history may miss this node)", err)
		} else {
			resp.Body.Close()
		}
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
		// завершит нас с tombstone'ом, как раньше.
		if ip != "" && cfg != nil {
			log.Printf("public IP unchanged (%s) — requesting gp re-verify from registry", ip)
			ok, retryAfter := register(client, cfg, ip)
			if ok || retryAfter > 0 {
				log.Printf("registry opened re-verify (ok=%t, retry_after=%s) — clearing tombstone, resuming service",
					ok, retryAfter.Round(time.Second))
				clearTombstone() // повторный kill, если придёт, положит новый
				return
			}
			log.Printf("re-verify request failed (registry unreachable?) — refusing to resurrect")
		}
		dieSelfFn(msgIPBan)
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
