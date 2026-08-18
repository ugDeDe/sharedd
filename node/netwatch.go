package main

// Сетевой вотчдог: ручная смена IP/шлюза на хостинге (vdsina и подобные)
// оставляет агента с протухшими keep-alive соединениями к регистратору —
// сокеты привязаны к старому исходящему адресу, все POST'ы (heartbeat,
// register, report) дохнут по Client.Timeout, хотя прокси жив и метрики
// зелёные. Плюс кэш публичного IP (5 мин TTL) подсовывает регистратору
// старый адрес при перерегистрации.
//
// Лечение по нарастающей:
//
//  1. Любой сетевой сбой обмена с регистратором → сброс idle keep-alive
//     соединений (транспорт общий у всех клиентов агента) — следующая
//     попытка уходит СВЕЖИМ сокетом, который ядро привяжет к новому
//     исходящему адресу.
//  2. Исходящий IPv4 сменился относительно последнего УСПЕШНОГО обмена →
//     дополнительно инвалидируется кэш публичного IP: перерегистрация
//     уйдёт уже с новым адресом.
//  3. Сбои продолжаются непрерывно ≥ netRestartAfter И исходящий IPv4
//     сменился → агент перезапускает сам себя: exit(1) под systemd,
//     Restart=always поднимет процесс через RestartSec с чистыми сокетами,
//     свежим детектом IP и немедленной регистрацией. Без systemd-юнита —
//     не выходим (иначе агент умрёт насовсем), продолжаем п.1-2.
//
// Перезапуск требует БАЗУ для сравнения (исходящий IP на момент успешного
// обмена), поэтому нода, стартовавшая в уже сломанной сети, в рестарт-цикл
// не уйдёт: после рестарта база пустая до первого успеха.

import (
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// netRestartAfter — сколько подряд должен быть недоступен регистратор
// (при сменившемся исходящем IP), прежде чем агент перезапустит сам себя.
// 45 c ≈ 3-4 heartbeat-тика: сбросу соединений даётся шанс всё починить.
const netRestartAfter = 45 * time.Second

// подменяются в тестах
var (
	detectOutboundIPv4Fn = detectOutboundIPv4
	restartSelfFn        = restartSelf
)

var netw = &netWatch{}

type netWatch struct {
	mu          sync.Mutex
	client      *http.Client // общий клиент агента — для CloseIdleConnections
	ipr         *ipResolver  // для сброса кэша публичного IP
	firstFailAt time.Time    // начало непрерывной серии сбоев (zero = сбоев нет)
	goodOutIP   string       // исходящий IPv4 на момент последнего успешного обмена
}

func (w *netWatch) bind(client *http.Client, ipr *ipResolver) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.client = client
	w.ipr = ipr
}

// resolver — общий ipResolver агента (ip_ban-ожидание переиспользует его,
// чтобы уважать ручной [node] public_ip).
func (w *netWatch) resolver() *ipResolver {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ipr
}

// noteOK — получен ЛЮБОЙ ответ регистратора (сеть жива): серия сбоев
// обнуляется, фиксируется текущий исходящий IPv4 как эталон.
func (w *netWatch) noteOK() {
	out, err := detectOutboundIPv4Fn()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.firstFailAt = time.Time{}
	if err == nil && out != "" {
		w.goodOutIP = out
	}
}

// noteFail — сетевой сбой обмена с регистратором. Всегда сбрасывает idle
// keep-alive соединения; при смене исходящего IPv4 инвалидирует кэш
// публичного IP, а при затяжной серии сбоев — перезапускает агента.
func (w *netWatch) noteFail() {
	now := time.Now()
	out, oerr := detectOutboundIPv4Fn()

	w.mu.Lock()
	if w.firstFailAt.IsZero() {
		w.firstFailAt = now
	}
	ipChanged := oerr == nil && out != "" && w.goodOutIP != "" && out != w.goodOutIP
	failingFor := now.Sub(w.firstFailAt)
	restartDue := ipChanged && failingFor >= netRestartAfter
	if restartDue {
		w.firstFailAt = now // перевзвод: без systemd попробуем ещё раз через netRestartAfter
	}
	from := w.goodOutIP
	client, ipr := w.client, w.ipr
	w.mu.Unlock()

	// Протухшие keep-alive: транспорт общий (http.DefaultTransport) у всех
	// клиентов агента, поэтому одного вызова достаточно для всех циклов.
	if client != nil {
		client.CloseIdleConnections()
	}
	if ipChanged {
		if ipr != nil {
			ipr.invalidate()
		}
		log.Printf("outbound IPv4 changed %s -> %s while registry is unreachable — dropped idle connections, public IP cache invalidated (will re-register from the new IP)",
			from, out)
	}
	if restartDue {
		restartSelfFn(fmt.Sprintf("outbound IPv4 changed %s -> %s and registry still unreachable after %s",
			from, out, failingFor.Round(time.Second)))
	}
}

// restartSelf — самоперезапуск через systemd: exit(1) при Restart=always
// поднимает агента заново (чистые сокеты, свежий детект IP, немедленная
// регистрация). Терминальные стопы (dieSelf) делают systemctl stop и выходят
// с 0 — эти семантики не пересекаются. Без systemd-юнита не выходим: убить
// агента насовсем хуже, чем продолжать ретраи со сбросом соединений.
func restartSelf(reason string) {
	if systemdAvailable() && unitLoaded(agentUnitName) {
		log.Printf("network change detected — restarting agent (%s); systemd will bring it back up", reason)
		exitProcess(1)
		return
	}
	log.Printf("network change detected (%s) — no systemd unit to restart under, continuing with fresh connections", reason)
}
