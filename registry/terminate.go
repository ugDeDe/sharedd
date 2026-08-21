package main

// Жёсткое завершение нод — заменяет бесконечные циклы
// «заболел → молчит → протух → перерегистрировался → снова заболел».
//
// Два терминальных класса, оба завершают ноду НАВСЕГДА (она не должна
// делать новую регистрацию) и фиксируются в БД (historyDB.bans):
//
// 1. ip_ban — нода не прошла globalping (БЕЗ предусловий по
// tcp/metrics): она уходит в КАРАНТИН (Candidate.Quarantine), регистратор
// продолжает верифицировать её measurement'ы; quarantine_attempts (ум. 3,
// правится из панели) подряд неудачных — бан по IP. Нода получает kill
// при ближайшем обращении (heartbeat/report/register: HTTP 403
// {"terminate":true,...}), пишет в лог сообщение MsgIPBan и останавливает
// службу. Выход из карантина ЛЮБЫМ путём, кроме верифицированного
// восстановления, считается баном: исчерпание попыток, expiry по
// heartbeat-TTL («нода отвалилась») и dead-окно tcp+metrics во время
// карантина — всё пишется в bans как ip_ban.
//
// Путей назад ДВА: регистрация с НОВОГО ip снимает запись
// сразу; регистрация с ТОГО ЖЕ (забаненного) ip — даёт одну
// GP-перепроверку (reverify): нода возвращается в карантин с последней
// попыткой, верифицированный ok снимает бан (ban_lifted), fail —
// возвращает в бан навсегда. Защита от ложных глобалпингов и отладки
// «прокси был выключен». В статистике один ip пишется баном ОДИН РАЗ:
// повторные баны адреса строк не добавляют, а восстановившийся адрес
// (reverify прошёл) свою строку из статистики убирает.
//
// 2. dead — TCP-порт не отвечает И метрики не поступают непрерывно
// дольше terminate_dead_min (ум. 10). Регистратор ставит терминальную
// запись (говорящая нода получит kill на следующем обращении); нода со
// своей стороны само-детектит тот же факт по локальным scrape'ам
// (node/terminate.go) и, умирая, шлёт POST /retire — так бан попадает
// в историю, даже если агент молчал и уже выпал по heartbeat-TTL.
// Бессрочен. Исключение: нода, умершая по tcp+metrics ВО
// ВРЕМЯ GP-карантина, записывается как ip_ban (её класс уже определён
// карантином); dead — только для нод, GP не фейливших.
//
// Транзиентное (меньше терминального окна) поведение СОХРАНЕНО:
// heartbeat-TTL expiry, prune-рипер с карантином 429 — для всех НЕтерминаль-
// ных смешанных классов красного. Каратинные ноды из-под prune выведены
// (их судьбу решает счётчик попыток, а не рипер).
//
// Учёт в статистике: строка в bans (публичный /dashboard, история
// банов панели) пишется, ТОЛЬКО если у ноды было время мастерства
// (hadMasterTime) — т.е. бан реально задел пользователей. Бан «пустого»
// адреса (reverify-перепроверка старого ip после лифта, dropout до первого
// назначения, конверсия ip в карантине без мастерства) в историю не
// попадает: иначе неуспешные перепроверки уже забаненных адресов
// накручивают счётчики, а бан этого адреса был учтён при первом бане.
// На само завершение, блоки и события политика не влияет.

import (
	"fmt"
	"log"
	"time"
)

// Точные тексты для лога ноды — ТЗ. Нода пишет их как есть (обёртка
// log.Print добавляет только штамп времени).
const (
	MsgIPBan = "Бан по ip, запустите службу заново после его смены"
	MsgDead  = "Регистратор не достучался до порта и/или не получил метрики"
)

// TerminatedRecord — терминальная запись по убитой ноде. Персистится в
// State (registry_state.json): рестарт регистратора блок не отменяет.
// Вечная история — в БД (bans), State — только оперативный блок-лист.
type TerminatedRecord struct {
	NodeID  string    `json:"node_id"`
	IP      string    `json:"ip"`
	Reason  string    `json:"reason"`  // BanReasonIPBan | BanReasonDead
	Message string    `json:"message"` // точный текст для лога агента
	At      time.Time `json:"at"`
	// ReverifyFailed — нода уже получала перепроверку старого ip
	// и провалила её: повторная попытка — только по кулдауну
	// reverifyCooldown (иначе цикл 410→register→карантин→бан вечен).
	ReverifyFailed bool `json:"reverify_failed,omitempty"`
	// StaleIP — запись поставлена автоматически при выходе ноды
	// из карантина со сменённым ip: блок привязан к СТАРОМУ ip, а нода жива
	// на новом. Такую запись не снимает terminateLiftIfIPChangedLocked
	// (нода ничьих инструкций не выполняла — она просто бросила плохой ip).
	StaleIP bool `json:"stale_ip,omitempty"`
}

// reverifyCooldown — минимальный зазор между проваленной GP-перепроверкой
// и следующей: внутри окна register со старого ip → 403 kill, после —
// шанс снова даётся («надолго не блокируем», но и не флапим).
const reverifyCooldown = 15 * time.Minute

// QuarantineState — нода в GP-карантине (всё зелёное, кроме globalping).
// Живёт внутри Candidate (персистится со state). Attempts — подряд
// неудачных НЕЗАВИСИМО верифицированных GP-проверок, включая ту, что
// привела в карантин; достиг cfg.QuarantineAttempts → бан.
type QuarantineState struct {
	EnteredAt         time.Time `json:"entered_at"`
	Attempts          int       `json:"attempts"`
	LastRatio         float64   `json:"last_ratio"`
	LastMeasurementID string    `json:"last_measurement_id,omitempty"`
	Stale             bool      `json:"stale,omitempty"`
	// Reverify — карантин посажен переподключением СТАРОГО
	// забаненного ip: попытка одна (Attempts посеяны как max-1), ok
	// снимает бан (ban_lifted), fail возвращает в бан навсегда
	// (запись получит ReverifyFailed — второй перепроверки не будет).
	Reverify bool `json:"reverify,omitempty"`
}

func banMessage(reason string) string {
	if reason == BanReasonDead {
		return MsgDead
	}
	return MsgIPBan
}

// hadMasterTime — успела ли нода поработать мастером: есть/был stint
// (закрытые секунды либо открытый MasterSince). Это условие записи бана
// в статистику: бан ноды, не задевшей пользователей, счётчики
// не двигает.
func (c *Candidate) hadMasterTime(now time.Time) bool {
	return c.MasterStints > 0 || c.MasterTimeSec(now) > 0
}

// terminateNodeLocked — окончательное завершение ноды: терминальная запись
// в State + строка в БД (вечная история) + событие + удаление кандидата
// (stint/назначения разрулит evaluateAssignments на ближайшем тике).
// lifetime — от RegisteredAt кандидата; для retire по уже выпавшей ноде
// кандидата нет — тогда вызывать terminateRetiredLocked. cause — контекст
// (почему именно сейчас: expiry из карантина, dead во время карантина…),
// пустой — обычный финал класса. Под write-lock r.mu.
func (r *Registry) terminateNodeLocked(c *Candidate, now time.Time, reason, cause string) {
	msg := banMessage(reason)
	if r.state.Terminated == nil {
		r.state.Terminated = make(map[string]*TerminatedRecord)
	}
	rec := &TerminatedRecord{NodeID: c.NodeID, IP: c.IP, Reason: reason, Message: msg, At: now}
	r.state.Terminated[c.NodeID] = rec
	// В историю — только бан ноды со временем мастерства; дедупликация по
	// ip в recordBan дополнительно гарантирует «один адрес — одна строка»,
	// даже если ре-бан случился после нового мастерства на перепроверке.
	if r.db != nil && c.hadMasterTime(now) {
		r.db.recordBan(banRow{
			TS: now, NodeID: c.NodeID, IP: c.IP, Reason: reason,
			LifetimeSec: int64(now.Sub(c.RegisteredAt).Seconds()),
		})
	}
	r.state.Counters.NodesTerminated++
	detail := fmt.Sprintf("%s (%s, lifetime %s)", msg, reason, now.Sub(c.RegisteredAt).Round(time.Second))
	if cause != "" {
		detail += " — " + cause
	}
	r.addEventLocked(Event{
		Type: EventNodeTerminated, NodeID: c.NodeID, IP: c.IP, Detail: detail,
	})
	delete(r.state.Candidates, c.NodeID)
	log.Printf("node %s (%s) TERMINATED: %s (%s)", c.NodeID, c.IP, msg, reason)
	r.persistStateLocked()
}

// terminateRetiredLocked — /retire от ноды, кандидата которой уже нет
// (протухла, пока молчала и лечилась). Терминальная запись всё равно
// нужна: без неё воскресший старый агент смог бы перерегистрироваться.
// В историю банов НЕ пишем: кандидата нет — время мастерства
// неизвестно, а пишутся только подтверждённо user-impacting баны.
func (r *Registry) terminateRetiredLocked(id, ip string, now time.Time, reason string) {
	if r.state.Terminated == nil {
		r.state.Terminated = make(map[string]*TerminatedRecord)
	}
	if _, dup := r.state.Terminated[id]; dup {
		return
	}
	r.state.Terminated[id] = &TerminatedRecord{
		NodeID: id, IP: ip, Reason: reason, Message: banMessage(reason), At: now,
	}
	r.state.Counters.NodesTerminated++
	r.addEventLocked(Event{
		Type: EventNodeTerminated, NodeID: id, IP: ip,
		Detail: banMessage(reason) + " (self-retire после выпадения из пула)",
	})
	log.Printf("retired node %s (%s) recorded as terminated (%s)", id, ip, reason)
	r.persistStateLocked()
}

// terminatedBlockingLocked — терминальная запись, БЛОКИРУЮЩАЯ обращение
// (id, ip), или nil. Для ip_ban обращение с ДРУГОГО ip не блокируется —
// оператор последовал инструкции «запустите службу заново после смены ip»;
// снятие записи оформляет /register (terminateLiftIfIPChangedLocked),
// heartbeat/report с нового ip просто получают «re-register» как обычно.
func (r *Registry) terminatedBlockingLocked(id, ip string) *TerminatedRecord {
	t := r.state.Terminated[id]
	if t == nil {
		return nil
	}
	if t.Reason == BanReasonIPBan && ip != "" && ip != t.IP {
		return nil
	}
	return t
}

// terminatedIPBanByIPLocked — ip_ban-запись по IP с ЛЮБЫМ node_id
// : переустановка агента генерирует новый id, но переподключение
// старого забаненного адреса тоже получает re-verify попытку. Под lock.
func (r *Registry) terminatedIPBanByIPLocked(ip string) *TerminatedRecord {
	for _, t := range r.state.Terminated {
		if t.Reason == BanReasonIPBan && t.IP == ip {
			return t
		}
	}
	return nil
}

// ApplyReverifyLocked — старый забаненный ip переподключился тем
// же адресом. Запись снимаем СЕЙЧАС (иначе heartbeat ноды получит kill до
// верификации), а ноду сажаем в карантин с ОДНОЙ решающей попыткой:
// верифицированный GP ok — ban_lifted, fail — terminate вернёт запись и
// kill дойдёт при следующем обращении. Под write-lock.
func (r *Registry) applyReverifyLocked(c *Candidate, rec *TerminatedRecord, now time.Time) {
	r.cfgMu.RLock()
	attempts := r.cfg.QuarantineAttempts
	r.cfgMu.RUnlock()
	last := attempts - 1
	if last < 0 {
		last = 0
	}
	c.Quarantine = &QuarantineState{EnteredAt: now, Attempts: last, Reverify: true}
	delete(r.state.Terminated, rec.NodeID)
	r.addEventLocked(Event{
		Type: EventNodeQuarantined, NodeID: c.NodeID, IP: c.IP,
		Detail: fmt.Sprintf("re-verify после gp-бана: ip не сменился (%s); одна верифицированная проверка решает — ok снимает бан, fail возвращает навсегда", rec.IP),
	})
	log.Printf("candidate %s (%s): same-ip re-register after gp ban — re-verify quarantine (attempt %d/%d)",
		c.NodeID, c.IP, last, attempts)
}

// reverifyOpenLocked — можно ли записи rec дать GP-перепроверку: ip_ban +
// (шанс не использован ЛИБО кулдаун после провала истёк). Под lock.
func reverifyOpenLocked(rec *TerminatedRecord, now time.Time) bool {
	if rec == nil || rec.Reason != BanReasonIPBan {
		return false
	}
	return !rec.ReverifyFailed || now.Sub(rec.At) >= reverifyCooldown
}

// QuarantineIPChangeLocked — нода сменила ip, будучи в карантине.
// Старый ip верифицированно провалил globalping (именно потому она и в
// карантине), и выход из карантина не восстановлением — это блокировка:
// строка bans(ip_ban) в вечной истории (п.3/п.5 ревью; — только
// если у ноды было время мастерства) + заградительная stale-запись по
// СТАРОМУ ip (кто бы ни пришёл с него — reverify-трек). Сама
// нода продолжает работу на новом ip: карантин снимаем, дальше её
// проверяет обычный цикл. Под write-lock.
func (r *Registry) quarantineIPChangeLocked(c *Candidate, newIP string, now time.Time) {
	oldIP := c.IP
	if r.db != nil && c.hadMasterTime(now) {
		r.db.recordBan(banRow{
			TS: now, NodeID: c.NodeID, IP: oldIP, Reason: BanReasonIPBan,
			LifetimeSec: int64(now.Sub(c.RegisteredAt).Seconds()),
		})
	}
	if r.state.Terminated == nil {
		r.state.Terminated = make(map[string]*TerminatedRecord)
	}
	r.state.Terminated[c.NodeID] = &TerminatedRecord{
		NodeID: c.NodeID, IP: oldIP, Reason: BanReasonIPBan, Message: MsgIPBan,
		At: now, StaleIP: true,
	}
	r.addEventLocked(Event{
		Type: EventIPBlocked, NodeID: c.NodeID, IP: oldIP,
		Detail: fmt.Sprintf("нода сменила ip в карантине %s -> %s — старый ip заблокирован (lifetime %s)",
			oldIP, newIP, now.Sub(c.RegisteredAt).Round(time.Second)),
	})
	c.Quarantine = nil
	log.Printf("candidate %s changed ip in quarantine %s -> %s — old ip recorded as ip_ban",
		c.NodeID, oldIP, newIP)
}

// terminateLiftIfIPChangedLocked — снятие ip_ban-блока при регистрации с
// нового IP. Вызывается в /register ДО prune-карантина. Под write-lock.
// Возвращает запись, если блок был снят.
func (r *Registry) terminateLiftIfIPChangedLocked(id, ip string, now time.Time) *TerminatedRecord {
	t := r.state.Terminated[id]
	if t == nil || t.Reason != BanReasonIPBan || ip == "" || ip == t.IP || t.StaleIP {
		return nil
	}
	delete(r.state.Terminated, id)
	r.addEventLocked(Event{
		Type: EventBanLifted, NodeID: id, IP: ip,
		Detail: fmt.Sprintf("ip changed %s -> %s after gp ban — termination lifted (ban stays in history)", t.IP, ip),
	})
	log.Printf("termination of %s lifted: ip changed %s -> %s", id, t.IP, ip)
	return t
}
