package main

import (
	"fmt"
	"log"
	"time"
)

// Рипер неактивных нод + карантин после prune.
//
// Раньше нода удалялась из пула ТОЛЬКО по протуханию heartbeat (expiryLoop).
// Дыра в модели: живой агент с мёртвым прокси шлёт heartbeat'ы и падающие
// отчёты вечно — нода висела в пуле «вся красная» бесконечно (heartbeat
// свежий, TCP/GP/metrics fail). Рипер: нода, непрерывно
// ВНЕ очереди здоровых дольше [healthcheck] prune_unhealthy_min (дефолт 60;
// явный 0 = выкл.), удаляется из пула событием node_pruned. Отсчёт — по
// Candidate.UnhealthySince (момент выхода из fully-healthy), персистится.
//
// Проблема: дверь получилась вертящейся — heartbeat по неизвестному id
// даёт 410, агент МГНОВЕННО пере-регистрируется, окно рипера стартует
// с нуля, и мёртвая нода фактически никуда не девается: она в пуле красная
// ~всё окно, а лента событий копит пары register/prune каждые N минут.
//
// Prune стал липким. После каждого prune пара (node_id, IP) ложится
// в карантин State.PruneStrikes: повторная регистрация отклоняется
// (429 + Retry-After), пока карантин не истечёт. Серия prune подряд (без
// единого fully-healthy эпизода между ними) удваивает карантин:
// 15m → 30m → 1h → 2h → 3h (потолок). Т.е. мёртвая нода пропадает из пула
// НАДОЛГО, а частота мусорных событий быстро затухает. Карантин сбрасывается,
// когда нода один раз реально входит в очередь здоровых (evaluateAssignments,
// ветка join). Совпадение по IP тоже учитывается: переустановка агента с
// новым node_id на том же хосте серию не отмывает.

const defaultPruneUnhealthyMinutes = 60

const (
	// pruneBanBase/pruneBanCap — расписание карантина: base * 2^(strikes-1),
	// не длиннее cap.
	pruneBanBase = 15 * time.Minute
	pruneBanCap  = 3 * time.Hour
	// tombstoneIdleTTL — сборка мусора: нода не объявлялась столько дней после
	// последнего prune — забываем её серию (переиспользованные/тестовые id).
	tombstoneIdleTTL = 7 * 24 * time.Hour
	// pruneMaxStrikes — предохранитель от переполнения сдвига в расписании.
	pruneMaxStrikes = 16
)

// PruneTombstone — карантинная запись по вычищенной рипером ноде. Персистится
// в state: рестарт регистратора карантин не отменяет.
type PruneTombstone struct {
	NodeID      string    `json:"node_id"`
	IP          string    `json:"ip,omitempty"`
	Strikes     int       `json:"strikes"` // серия prune подряд (→ длина карантина)
	LastPruned  time.Time `json:"last_pruned"`
	BannedUntil time.Time `json:"banned_until"` // до этого момента /register отклоняется 429
}

// pruneBanFor — длина карантина для k-го strike (k >= 1): 15m, 30m, 1h, 2h,
// 3h, 3h, … — потолок pruneBanCap.
func pruneBanFor(strikes int) time.Duration {
	if strikes < 1 {
		strikes = 1
	}
	if strikes > pruneMaxStrikes {
		strikes = pruneMaxStrikes
	}
	d := pruneBanBase << (strikes - 1)
	if d > pruneBanCap || d <= 0 {
		d = pruneBanCap
	}
	return d
}

// resolvePruneUnhealthyMinutes — эффективное окно (мин): nil → дефолт,
// 0 → рипер выключен. Семантика указателя как у master_ttl_minutes.
func resolvePruneUnhealthyMinutes(p *int) int {
	if p == nil {
		return defaultPruneUnhealthyMinutes
	}
	return *p
}

// effectiveTombstoneLocked — самый строгий карантин для (id, ip): прямая
// запись по node_id либо наследство прошлой регистрации с тем же IP.
// Возвращает nil, если активного карантина нет.
func (r *Registry) effectiveTombstoneLocked(id, ip string, now time.Time) *PruneTombstone {
	var best *PruneTombstone
	for _, tb := range r.state.PruneStrikes {
		if tb.NodeID != id && (ip == "" || tb.IP != ip) {
			continue
		}
		if !now.Before(tb.BannedUntil) {
			continue // карантин истёк — не блокирует
		}
		if best == nil || tb.BannedUntil.After(best.BannedUntil) {
			best = tb
		}
	}
	return best
}

// tombstoneStrikesForLocked — серия, которую продолжит (id, ip) при новом
// prune: максимум strikes по записям этого id И этого IP (переустановка
// агента меняет node_id, но не IP хоста — серия не отмывается).
func (r *Registry) tombstoneStrikesForLocked(id, ip string) int {
	n := 0
	for _, tb := range r.state.PruneStrikes {
		if tb.NodeID == id || (ip != "" && tb.IP == ip) {
			if tb.Strikes > n {
				n = tb.Strikes
			}
		}
	}
	return n
}

// upsertTombstoneLocked — нода уходит в prune: серия +1, карантин по
// расписанию. Запись живёт по node_id; старые записи того же IP с меньшей
// серией подчищаются (сильнейшая уже учтена в новой).
func (r *Registry) upsertTombstoneLocked(id, ip string, now time.Time) *PruneTombstone {
	if r.state.PruneStrikes == nil {
		r.state.PruneStrikes = make(map[string]*PruneTombstone)
	}
	strikes := r.tombstoneStrikesForLocked(id, ip) + 1
	for key, tb := range r.state.PruneStrikes {
		if key != id && ip != "" && tb.IP == ip && tb.Strikes < strikes {
			delete(r.state.PruneStrikes, key)
		}
	}
	tb := r.state.PruneStrikes[id]
	if tb == nil {
		tb = &PruneTombstone{NodeID: id}
		r.state.PruneStrikes[id] = tb
	}
	tb.IP = ip
	tb.Strikes = strikes
	tb.LastPruned = now
	tb.BannedUntil = now.Add(pruneBanFor(strikes))
	return tb
}

// clearTombstonesOnJoinLocked — нода реально вошла в очередь здоровых:
// серия оборвалась, карантин забывается (и её запись, и наследство по IP).
func (r *Registry) clearTombstonesOnJoinLocked(c *Candidate) {
	for key, tb := range r.state.PruneStrikes {
		if tb.NodeID == c.NodeID || (c.IP != "" && tb.IP == c.IP) {
			log.Printf("prune strikes for %s (%s) cleared — node is healthy again (was %d)",
				c.NodeID, c.IP, tb.Strikes)
			delete(r.state.PruneStrikes, key)
		}
	}
}

// sweepExpired — удаление нод из пула. Два независимых основания:
//  1. heartbeat протух (агент умер/отрезан) — классический expiry;
//  2. нода непрерывно вне очереди здоровых дольше PruneUnhealthyTTL —
//     рипер, с карантином на возврат.
//
// Заодно собирает мусор в PruneStrikes (idle-записи старше tombstoneIdleTTL).
func (r *Registry) sweepExpired(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	changed := false
	for id, c := range r.state.Candidates {
		if now.Sub(c.LastHeartbeat) > r.cfg.HeartbeatTTL {
			// Нода отвалилась ИЗ КАРАНТИНА — это не «просто
			// expiry», а бан: она в карантине потому, что не прошла
			// globalping. Пишем ip_ban (в статистику дашборда входит).
			if c.Quarantine != nil && c.Quarantine.Attempts > 0 {
				r.terminateNodeLocked(c, now, BanReasonIPBan, "нода отвалилась из карантина (heartbeat TTL истёк)")
				changed = true
				continue
			}
			log.Printf("candidate %s expired (no heartbeat), removing", id)
			// stint/назначения закроет/перераздаст evaluateAssignments
			r.addEventLocked(Event{
				Type: EventNodeExpired, NodeID: id, IP: c.IP,
				Detail: fmt.Sprintf("no heartbeat for %s", now.Sub(c.LastHeartbeat).Round(time.Second)),
			})
			delete(r.state.Candidates, id)
			changed = true
			continue
		}
		//, класс dead: TCP-порт не отвечает И метрик нет (отчёты
		// протухли ИЛИ серия отчётов красная) непрерывно TerminateDeadTTL →
		// терминальное завершение. Отсчёт непрерывности — DeadBothSince:
		// зеленение любой ноги окно обнуляет. Молчащие ноды до этого окна
		// обычно не доживают (heartbeat-TTL меньше) — их добывает агентское
		// само-завершение + /retire; этот путь — для говорящих нод и
		// старых агентов.
		if r.cfg.TerminateDeadTTL > 0 {
			bothDead := !c.Healthy &&
				(!c.MetricsHealthy || c.LastReportAt.IsZero() || now.Sub(c.LastReportAt) > r.cfg.ReportFreshnessTTL)
			if bothDead && c.DeadBothSince.IsZero() {
				c.DeadBothSince = now
				changed = true
			} else if !bothDead && !c.DeadBothSince.IsZero() {
				c.DeadBothSince = time.Time{}
			}
			if bothDead && now.Sub(c.DeadBothSince) >= r.cfg.TerminateDeadTTL {
				// Dead ВО ВРЕМЯ карантина — записываем как ip_ban
				// (нода уже в карантине за GP-фейл); «чистый» dead — только
				// для нод, GP не фейливших.
				reason, cause := BanReasonDead, ""
				if c.Quarantine != nil && c.Quarantine.Attempts > 0 {
					reason, cause = BanReasonIPBan, "dead (tcp+metrics) во время gp-карантина"
				}
				r.terminateNodeLocked(c, now, reason, cause) // удаляет кандидата + persist
				changed = true
				continue
			}
		}
		if r.cfg.PruneUnhealthyTTL > 0 && !c.FullyHealthy && c.Quarantine == nil &&
			!c.UnhealthySince.IsZero() && now.Sub(c.UnhealthySince) >= r.cfg.PruneUnhealthyTTL {
			// Карантинных (c.Quarantine != nil) рипер не трогает —
			// их судьбу решает счётчик GP-попыток (terminate по ip_ban).
			tb := r.upsertTombstoneLocked(id, c.IP, now)
			log.Printf("candidate %s pruned (out of healthy queue for %s) — re-register banned for %s (strike %d)",
				id, now.Sub(c.UnhealthySince).Round(time.Second), pruneBanFor(tb.Strikes), tb.Strikes)
			r.addEventLocked(Event{
				Type: EventNodePruned, NodeID: id, IP: c.IP,
				Detail: fmt.Sprintf("unhealthy for %s (%s) — pruned; re-register banned for %s (strike %d)",
					now.Sub(c.UnhealthySince).Round(time.Second),
					c.unhealthyReason(r.cfg.ReportFreshnessTTL),
					pruneBanFor(tb.Strikes), tb.Strikes),
			})
			delete(r.state.Candidates, id)
			changed = true
		}
	}
	for key, tb := range r.state.PruneStrikes {
		if now.Sub(tb.LastPruned) > tombstoneIdleTTL {
			delete(r.state.PruneStrikes, key)
			changed = true
		}
	}
	if changed {
		r.persistStateLocked()
	}
}
