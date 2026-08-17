package main

import (
	"log"
	"sync"
	"time"
)

// Гейт молчания ноды (; унификация защёлок; — GP-нога
// снята, добавлен dead-килл).
//
// Heartbeat регистратору означает «обслуживаю», а не «жив процесс агента»:
// нода с красными локальными scrape'ами telemt /metrics полностью замолкает
// (ни отчётов, ни heartbeat) и сама снимается с пула по heartbeat_ttl.
// Локальные проверки при этом продолжаются — это и есть лечение: позеленело
// → первый heartbeat ловит 410 → пере-регистрация → возврат за секунды.
//
// GP-нога немоты УДАЛЕНА. Судьба «всё зелёное, кроме globalping»
// теперь терминальная и живёт на регистраторе (GP-карантин → N попыток →
// бан по IP, terminate.go): чтобы kill-сигнал можно было ДОСТАВИТЬ, нода
// при плохом GP обязана продолжать heartbeat/отчёты, а не молчать.
//
// Немота по метрикам отныне с часами — netGate.mMutedSince:
// непрерывные красные scrape'ы дольше watchdog.dead_kill_ms (ум. 10 мин) =
// терминальный класс dead, агент завершается сам (terminate.go), в лог —
// msgDead. Транзиентные эпизоды короче окна ведут себя ровно как.
//
// Сюда же сведён prune-карантин (429 + Retry-After с регистратора,):
// пока бан активен — то же полное молчание до дедлайна, даже если всё
// зелёное (dead-килл на карантинный бан НЕ распространяется: часы крутятся
// только по метрик-ноге).
const (
	failSilenceThreshold    = 3 // подряд плохих scrape'ов до немоты
	recoverSilenceThreshold = 2 // подряд хороших до возврата
)

// streakStep — один шаг анти-флап защёлки «failK подряд плохих гасит,
// recoverK подряд хороших возвращает». Точь-в-точь такая же машина живёт на
// регистраторе (TCP-пробы и metrics-отчёты). Счётчики серий после
// переключения не обрезаются — наружу выходит только факт переключения.
func streakStep(on bool, fails, oks int, ok bool, failK, recoverK int) (bool, int, int, bool) {
	if ok {
		fails = 0
		oks++
		if !on && oks >= recoverK {
			return true, 0, oks, true
		}
		return on, fails, oks, false
	}
	oks = 0
	fails++
	if on && fails >= failK {
		return false, fails, 0, true
	}
	return on, fails, oks, false
}

// netGate — единая точка решения «молчим ли мы для регистратора». Питается
// от metricsLoop (локальные scrape'ы) и heartbeatLoop (prune-карантин);
// читается всеми сетевыми циклами. Потокобезопасен, нулевое значение сразу
// пригодно («здоров» — false).
type netGate struct {
	mu sync.Mutex

	mMuted      bool      // нога метрик: true = scrape telemt /metrics красный (немота)
	mFails      int       // текущая серия подряд плохих
	mOKs        int       // текущая серия подряд хороших
	mMutedSince time.Time // непрерывная метрик-немота с часов (dead-килл); ноль — не молчим

	banUntil time.Time // prune-карантин с регистратора (429 Retry-After)
}

var gate = &netGate{}

// noteLocal — результат очередного scrape telemt /metrics (ok=false при
// ошибке scrape или красном вердикте). Защёлка немоты + часы dead-килла.
func (g *netGate) noteLocal(ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	on, nf, no, changed := streakStep(!g.mMuted, g.mFails, g.mOKs, ok,
		failSilenceThreshold, recoverSilenceThreshold)
	muted := !on
	g.mMuted, g.mFails, g.mOKs = muted, nf, no
	if changed && muted {
		g.mMutedSince = time.Now()
		log.Printf("metrics scrape failing %d times in a row — entering SILENT HEALING mode: heartbeat/reports stop until the proxy is locally green again (registry will drop the node by heartbeat TTL)",
			failSilenceThreshold)
	} else if changed {
		g.mMutedSince = time.Time{}
		log.Printf("silent healing over (metrics): %d consecutive green scrapes — resuming registry traffic (heartbeat -> re-register)",
			recoverSilenceThreshold)
	}
}

// deadKillDue — терминальный dead: метрик-немота держится непрерывно дольше
// after. Вызывается metricsLoop на каждом тике; при true вызывающий обязан
// завершить ноду (terminate.go).
func (g *netGate) deadKillDue(now time.Time, after time.Duration) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.mMuted && !g.mMutedSince.IsZero() && now.Sub(g.mMutedSince) >= after
}

func (g *netGate) metricsMuted() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.mMuted
}

func (g *netGate) banActive() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return time.Now().Before(g.banUntil)
}

// silent — молчим ли мы для регистратора прямо сейчас (метрик-немота или бан).
func (g *netGate) silent() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.mMuted || time.Now().Before(g.banUntil)
}

// noteBan — регистратор поставил карантин (429 + Retry-After на /register).
// Молчим минимум до этого дедлайна, даже если локально всё зелёное.
// Поздний короткий бан активный длинный не укорачивает.
func (g *netGate) noteBan(until time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if until.After(g.banUntil) {
		g.banUntil = until
	}
}
