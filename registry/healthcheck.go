package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"
)

type HealthReportPayload struct {
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

// streakStep — один шаг анти-флап защёлки «failK подряд плохих гасит,
// recoverK подряд хороших возвращает» (TCP-пробы и metrics-отчёты — одна и
// та же машина; до V7.9.10 существовала в двух рукописных копиях). Счётчики
// серий не обрезаются после смены состояния: панель показывает «серия N/F»
// во время набега и полную длину серии вне окна. Возвращает новые
// (on, fails, oks) и changed=true при смене состояния — события up/down и
// логи остаются на стороне вызывающего (им текст нужен streak-счётчик).
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

// clampThreshold — пороги защёлок смысла «0/отсутствует» не имеют (0 = гасить
// сразу — НЕ дефолт); пустые значения конфига выравниваем к 1. max(1, …) в
// точках хендлеров исторически писался вручную — сведено сюда.
func clampThreshold(v int) int { return max(1, v) }

func (r *Registry) handleHealthReport(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload HealthReportPayload
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if payload.NodeID == "" {
		http.Error(w, "node_id required", http.StatusBadRequest)
		return
	}

	r.mu.Lock()
	candidate, ok := r.state.Candidates[payload.NodeID]
	// V7.9.11: отчёт от терминально убитой ноды — kill-сигнал вместо 404.
	var rec *TerminatedRecord
	if !ok {
		host, _, _ := net.SplitHostPort(req.RemoteAddr)
		rec = r.terminatedBlockingLocked(payload.NodeID, host)
	}
	r.mu.Unlock()
	if rec != nil {
		log.Printf("report from terminated node %s — sending kill (%s)", payload.NodeID, rec.Reason)
		r.writeTerminate(w, rec)
		return
	}
	if !ok {
		http.Error(w, "unknown node_id, register first", http.StatusNotFound)
		return
	}

	// Отчёт бывает двух видов (тайминги globalping/metrics разделены на ноде):
	// с measurement_id — от globalping-цикла, без него — от metrics-цикла.
	// Без measurement_id ранее верифицированный globalping-статус НЕ затираем.
	verifyGP := payload.GlobalpingMeasurementID != ""
	verifiedRatio := 0.0
	verifiedOK := false
	// V7.9.1: верификация «не состоялась» (API недоступно / measurement ещё
	// выполняется) НЕ переводит ноду в заблокированные — иначе сетевой чих до
	// globalping рисовал ratio 0. GP-статус просто сохраняется до следующего цикла.
	verifiedKnown := false
	verifyErr := ""
	var measurement *globalpingMeasurement // V7.7: сохраняем для детали площадок
	if verifyGP {
		// api_base Globalping — под cfgMu (панель может менять на лету)
		r.cfgMu.RLock()
		gpBase := r.cfg.Globalping.APIBase
		r.cfgMu.RUnlock()
		gp := NewGlobalpingChecker(gpBase)
		m, err := gp.FetchFinished(payload.GlobalpingMeasurementID, 45*time.Second)
		if err != nil && m == nil {
			// чистая ошибка скачивания (сеть/5xx) — один повтор через 3 с
			time.Sleep(3 * time.Second)
			m, err = gp.FetchFinished(payload.GlobalpingMeasurementID, 30*time.Second)
		}
		if err != nil {
			verifyErr = err.Error()
			log.Printf("globalping verification of %s for %s INCONCLUSIVE: %v — keeping previous GP state",
				payload.GlobalpingMeasurementID, payload.NodeID, err)
		} else {
			verifiedKnown = true
			measurement = m
			verifiedRatio = evaluateSuccessRatio(measurement)
			verifiedOK = verifiedRatio >= 0.5
			if verifiedOK != payload.GlobalpingOK {
				log.Printf("WARNING: node %s reported globalping_ok=%v but independent verification says %v (ratio=%.2f) — trusting verification",
					payload.NodeID, payload.GlobalpingOK, verifiedOK, verifiedRatio)
			}
		}
	}

	r.mu.Lock()
	candidate.Port = payload.Port
	candidate.ReportsTotal++
	r.state.Counters.HealthReports++
	// Отчёт засчитываем как "хороший" только если всё, что он несёт, в норме:
	// metrics-отчёт — по MetricsOK, globalping-отчёт — по обоим. Несостоявшаяся
	// верификация (inconclusive) отчёт не портит — нода тут ни при чём.
	reportOK := payload.MetricsOK && (!verifyGP || !verifiedKnown || verifiedOK)
	if reportOK {
		candidate.ReportsOK++
	}
	if verifyGP && verifiedKnown {
		prevGP := candidate.GlobalpingOK
		candidate.GlobalpingOK = verifiedOK
		candidate.GlobalpingMeasurementID = payload.GlobalpingMeasurementID
		candidate.GlobalpingVerifiedRatio = verifiedRatio
		candidate.GPChecksTotal++
		if verifiedOK {
			candidate.GPChecksOK++
		}
		switch {
		case prevGP && !verifiedOK:
			// нода заблокирована/недоступна снаружи — судим по НЕЗАВИСИМОЙ
			// проверке Globalping, а не по самоотчёту ноды
			detail := fmt.Sprintf("verified ratio %.2f (measurement %s)", verifiedRatio, payload.GlobalpingMeasurementID)
			if payload.GlobalpingOK {
				detail += "; node self-reported ok"
			}
			r.state.Counters.GPBlocked++
			r.addEventLocked(Event{Type: EventGlobalpingBlocked, NodeID: candidate.NodeID, IP: candidate.IP, Detail: detail})
		case !prevGP && verifiedOK:
			r.addEventLocked(Event{
				Type: EventGlobalpingRecovered, NodeID: candidate.NodeID, IP: candidate.IP,
				Detail: fmt.Sprintf("verified ratio %.2f", verifiedRatio),
			})
		}
		// V7.7: история + деталь по площадкам для страницы статистики ноды.
		// Деталь — только когда measurement реально скачан (verifyErr пуст).
		probesOK, probesTotal := 0, 0
		if measurement != nil {
			lines := make([]GPProbeLine, 0, len(measurement.Results))
			for _, res := range measurement.Results {
				ok := probeResultOK(res)
				if ok {
					probesOK++
				}
				lines = append(lines, GPProbeLine{
					Country:  res.Probe.Country,
					City:     res.Probe.City,
					Network:  res.Probe.Network,
					ASN:      res.Probe.ASN,
					OK:       ok,
					Status:   res.Result.Status,
					HTTPCode: res.Result.StatusCode,
				})
			}
			probesTotal = len(measurement.Results)
			candidate.GPLast = &GPDetail{
				At: time.Now(), MeasurementID: payload.GlobalpingMeasurementID,
				OK: verifiedOK, Ratio: verifiedRatio, ProbesOK: probesOK,
				ProbesTotal: probesTotal, Probes: lines,
			}
		}
		candidate.GPHist = pushRing(candidate.GPHist, GPPoint{
			At: time.Now(), OK: verifiedOK, Ratio: verifiedRatio,
			ProbesOK: probesOK, ProbesTotal: probesTotal,
		}, gpHistCap)

		// GP-карантин. V7.9.12: вход — по ЛЮБОМУ верифицированному GP-фейлу
		// (без предусловия «всё зелёное»: не прошла globalping — в карантин).
		// Дальше каждая неудачная ВЕРИФИЦИРОВАННАЯ проверка увеличивает
		// счётчик попыток; достиг quarantine_attempts (правится из панели) —
		// нода завершается баном по IP (kill при следующем её обращении).
		// Верифицированный зелёный отчёт в карантине — честное восстановление,
		// карантин снимается (бан не засчитывается); для reverify-карантина
		// (старый забаненный ip) это ещё и снятие бана (ban_lifted).
		switch {
		case verifiedOK && candidate.Quarantine != nil:
			q := candidate.Quarantine
			r.addEventLocked(Event{
				Type: EventQuarantineRecovered, NodeID: candidate.NodeID, IP: candidate.IP,
				Detail: fmt.Sprintf("verified ok after %d failed attempt(s)", q.Attempts),
			})
			log.Printf("candidate %s left gp quarantine (recovered after %d attempts)", candidate.NodeID, q.Attempts)
			candidate.Quarantine = nil
			if q.Reverify {
				r.addEventLocked(Event{
					Type: EventBanLifted, NodeID: candidate.NodeID, IP: candidate.IP,
					Detail: "старый ip прошёл gp re-verify — бан снят (в истории банов остаётся)",
				})
				log.Printf("candidate %s: gp re-verify ok — old ip unbanned", candidate.NodeID)
			}
		case !verifiedOK && candidate.Quarantine != nil:
			candidate.Quarantine.Attempts++
			candidate.Quarantine.LastRatio = verifiedRatio
			candidate.Quarantine.LastMeasurementID = payload.GlobalpingMeasurementID
			if candidate.Quarantine.Attempts >= r.cfg.QuarantineAttempts {
				reverifyFail := candidate.Quarantine.Reverify
				cause := "карантин исчерпан"
				if reverifyFail {
					cause = "re-verify после gp-бана провален"
				}
				r.terminateNodeLocked(candidate, time.Now(), BanReasonIPBan, cause) // удаляет кандидата + persist
				if reverifyFail {
					// шанс использован и провален — дальше только 403 kill
					r.state.Terminated[candidate.NodeID].ReverifyFailed = true
				}
				r.mu.Unlock()
				log.Printf("health report from %s: gp quarantine attempt %d/%d failed — node terminated (ip ban)",
					payload.NodeID, candidate.Quarantine.Attempts, r.cfg.QuarantineAttempts)
				w.WriteHeader(http.StatusOK)
				return
			}
			log.Printf("candidate %s gp quarantine: failed attempt %d/%d (ratio=%.2f)",
				candidate.NodeID, candidate.Quarantine.Attempts, r.cfg.QuarantineAttempts, verifiedRatio)
		case !verifiedOK:
			candidate.Quarantine = &QuarantineState{
				EnteredAt: time.Now(), Attempts: 1, LastRatio: verifiedRatio,
				LastMeasurementID: payload.GlobalpingMeasurementID,
			}
			r.addEventLocked(Event{
				Type: EventNodeQuarantined, NodeID: candidate.NodeID, IP: candidate.IP,
				Detail: fmt.Sprintf("globalping fail verified (ratio %.2f; tcp_ok=%t, metrics_ok=%t) — quarantine, attempt 1/%d",
					verifiedRatio, candidate.Healthy, candidate.MetricsHealthy, r.cfg.QuarantineAttempts),
			})
			log.Printf("candidate %s entered gp quarantine (verified fail; tcp_ok=%t metrics_ok=%t) — attempt 1/%d",
				candidate.NodeID, candidate.Healthy, candidate.MetricsHealthy, r.cfg.QuarantineAttempts)
		}
	}
	candidate.MetricsOK = payload.MetricsOK
	// V7.9.4: metrics-защёлка по fail/recover-порогам (аналог TCP-защёлки
	// Healthy в probeLoop). Одиночный сбойный отчёт fully-healthy НЕ роняет:
	// отказ фиксируется только после fail_threshold ПОДРЯД плохих отчётов,
	// возврат в строй — после recover_threshold подряд хороших. Серия ведётся
	// по ВСЕМ отчётам ноды (metrics-цикл раз в metrics_ms; globalping-цикл
	// тоже несёт свежий metrics-вердикт — mix ок, оба отражают живость
	// локального /metrics). Отдельно существует dead-man's switch:
	// report_freshness_min без единого отчёта гасит ноду независимо от серий.
	{
		failThr := clampThreshold(r.cfg.Healthcheck.FailThreshold)
		recThr := clampThreshold(r.cfg.Healthcheck.RecoverThreshold)
		var changed bool
		candidate.MetricsHealthy, candidate.MetricsFailStreak, candidate.MetricsOKStreak, changed =
			streakStep(candidate.MetricsHealthy, candidate.MetricsFailStreak, candidate.MetricsOKStreak,
				payload.MetricsOK, failThr, recThr)
		if changed && candidate.MetricsHealthy {
			r.addEventLocked(Event{
				Type: EventMetricsUp, NodeID: candidate.NodeID, IP: candidate.IP,
				Detail: fmt.Sprintf("%d consecutive ok reports", candidate.MetricsOKStreak),
			})
			log.Printf("candidate %s metrics healthy again (%d consecutive ok)", candidate.NodeID, candidate.MetricsOKStreak)
		} else if changed {
			detail := fmt.Sprintf("%d consecutive failed metrics reports", candidate.MetricsFailStreak)
			if payload.Error != "" {
				detail += "; last: " + payload.Error
			}
			r.addEventLocked(Event{Type: EventMetricsDown, NodeID: candidate.NodeID, IP: candidate.IP, Detail: detail})
			log.Printf("candidate %s metrics unhealthy: %s", candidate.NodeID, detail)
		}
	}
	if payload.MetricsSnapshot != nil {
		candidate.MetricsSnapshot = payload.MetricsSnapshot
	}
	// V7.7: история metrics-отчётов (клиенты/райтеры) для страницы ноды
	{
		clients := -1
		if v, ok := payload.MetricsSnapshot[uniqueIPsMetric]; ok {
			clients = int(v)
		}
		candidate.ReportHist = pushRing(candidate.ReportHist, ReportPoint{
			At: time.Now(), MetricsOK: payload.MetricsOK,
			Clients: clients, Writers: int(payload.MetricsSnapshot[writersMetric]),
		}, reportHistCap)
	}
	candidate.LastReportAt = time.Now()
	candidate.ReportError = payload.Error
	r.persistStateLocked()
	r.mu.Unlock()

	switch {
	case verifyGP && !verifiedKnown:
		log.Printf("health report from %s: metrics=%v (globalping verification inconclusive: %s — GP state kept)",
			payload.NodeID, payload.MetricsOK, verifyErr)
	case verifyGP:
		log.Printf("health report from %s: globalping_verified=%v (ratio=%.2f) metrics=%v",
			payload.NodeID, verifiedOK, verifiedRatio, payload.MetricsOK)
	default:
		log.Printf("health report from %s: metrics=%v (globalping unchanged)", payload.NodeID, payload.MetricsOK)
	}
	w.WriteHeader(http.StatusOK)
}

func (c *Candidate) IsFullyHealthy(freshnessTTL time.Duration) bool {
	if !c.Healthy {
		return false
	}
	// V7.9.4: вместо сырого MetricsOK последнего отчёта — защёлка
	// MetricsHealthy (fail/recover-пороги). GlobalpingOK остаётся
	// мгновенным: это НЕЗАВИСИМАЯ верификация регистратора, нода её
	// подделать не может, а подтверждённо заблокированный мастер —
	// мёртвый груз для домена, задержка ротации тут только вредит.
	if !c.GlobalpingOK || !c.MetricsHealthy {
		return false
	}
	if c.LastReportAt.IsZero() || time.Since(c.LastReportAt) > freshnessTTL {
		return false
	}
	return true
}

// unhealthyReason — человекочитаемая первопричина того, что нода НЕ fully
// healthy. Используется в журнале событий (queue_left) и панели.
func (c *Candidate) unhealthyReason(freshnessTTL time.Duration) string {
	switch {
	case c.Quarantine != nil: // V7.9.11
		return fmt.Sprintf("gp quarantine: failed verified attempt %d (last ratio %.2f) — awaiting ban verdict or recovery",
			c.Quarantine.Attempts, c.Quarantine.LastRatio)
	case !c.Healthy:
		return "tcp probe failing (port unreachable)"
	case !c.GlobalpingOK:
		reason := "globalping verification failed (blocked/unreachable from outside)"
		if c.GlobalpingMeasurementID != "" {
			reason += fmt.Sprintf(", ratio %.2f", c.GlobalpingVerifiedRatio)
		}
		return reason
	case !c.MetricsHealthy:
		// V7.9.4: сюда попадаем только после серии плохих отчётов (защёлка)
		reason := fmt.Sprintf("telemt metrics failing (%d consecutive bad reports hit fail_threshold)", c.MetricsFailStreak)
		if c.ReportError != "" {
			reason += ": " + c.ReportError
		}
		return reason
	case c.LastReportAt.IsZero():
		return "no health report received yet"
	case time.Since(c.LastReportAt) > freshnessTTL:
		return fmt.Sprintf("health report stale (%s ago)", time.Since(c.LastReportAt).Round(time.Second))
	default:
		return "unknown"
	}
}

// AvailabilityPct — доля успешных health-отчётов ноды за всё время (0..100).
func (c *Candidate) AvailabilityPct() float64 {
	if c.ReportsTotal == 0 {
		return 0
	}
	return float64(c.ReportsOK) * 100 / float64(c.ReportsTotal)
}

// GPVerifiedPct — доля успешных НЕЗАВИСИМЫХ проверок Globalping (0..100).
func (c *Candidate) GPVerifiedPct() float64 {
	if c.GPChecksTotal == 0 {
		return 0
	}
	return float64(c.GPChecksOK) * 100 / float64(c.GPChecksTotal)
}

// MasterTimeSec — полное время в роли мастера: закрытые stint'ы + текущий.
func (c *Candidate) MasterTimeSec(now time.Time) int64 {
	total := c.MasterSeconds
	if !c.MasterSince.IsZero() {
		total += int64(now.Sub(c.MasterSince).Seconds())
	}
	return total
}
