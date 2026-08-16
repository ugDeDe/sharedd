package main

import "time"

// Журнал событий регистратора: история блокировок, смен мастера, жизненный
// цикл нод. Хранится кольцевым буфером внутри State (персистится вместе с
// состоянием на диск), лимит — cfg.Panel.EventsMax.

const (
	EventRegistryStarted     = "registry_started"
	EventNodeRegistered      = "node_registered"
	EventNodeReplaced        = "node_replaced" // вытеснена регистрацией с тем же IP
	EventNodeExpired         = "node_expired"  // heartbeat TTL истёк, нода удалена
	EventNodePruned          = "node_pruned"   // непрерывно нездорова дольше prune_unhealthy_min — удалена
	EventTCPDown             = "tcp_down"
	EventTCPUp               = "tcp_up"
	EventMetricsDown         = "metrics_down"         // защёлка метрик закрылась (fail_threshold подряд плохих отчётов)
	EventMetricsUp           = "metrics_up"           // защёлка метрик открылась (recover_threshold подряд хороших)
	EventGlobalpingBlocked   = "globalping_blocked"   // независимая проверка Globalping: фейл
	EventGlobalpingRecovered = "globalping_recovered" // проверка снова проходит
	EventQueueJoined         = "queue_joined"         // вошла в очередь мастерства (fully healthy)
	EventQueueLeft           = "queue_left"           // выпала из очереди (позиция сгорела)
	EventMasterElected       = "master_elected"
	EventMasterLost          = "master_lost"
	EventDNSUpdated          = "dns_updated"
	EventDNSError            = "dns_error"
	EventDNSDeleted          = "dns_deleted"    // записи домена, выведенного из managed-списка, удалены из Cloudflare
	EventConfigChanged       = "config_changed" // секции конфига изменены через панель
	// GP-карантин и терминальное завершение нод (terminate.go).
	EventNodeQuarantined     = "node_quarantined"     // всё зелёное кроме GP — отдельная таблица, ждёт вердикта
	EventQuarantineRecovered = "quarantine_recovered" // GP в карантине позеленел — нода возвращается нормально
	EventNodeTerminated      = "node_terminated"      // финал: бан (ip_ban/dead), нода мертва, регистраций нет
	EventBanLifted           = "ban_lifted"           // ip_ban снят: служба перезапущена с нового IP (история бана сохраняется)
	EventIPBlocked           = "ip_blocked"           // нода сменила ip в карантине — старый ip записан как бан
	// СРМД — масштабирование пула доменов (srmd.go).
	EventSRMDDomainCreated  = "srmd_domain_created"  // создан сиротский домен с инкрементом
	EventSRMDDomainFolded   = "srmd_domain_folded"   // лишний домен свёрнут в CNAME на оставшийся
	EventSRMDDomainUnfolded = "srmd_domain_unfolded" // свёрнутый домен возвращён в ротацию мастеров
	EventSRMDDomainTaken    = "srmd_domain_taken"    // ручной домен взят под контроль СРМД
	EventSRMDDomainReleased = "srmd_domain_released" // домен СРМД переведён обратно в ручной режим
)

type Event struct {
	At     time.Time `json:"at"`
	Type   string    `json:"type"`
	NodeID string    `json:"node_id,omitempty"`
	IP     string    `json:"ip,omitempty"`
	// Domain — к какому managed-домену относится событие (мастера per-domain).
	Domain string `json:"domain,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Counters — накопительные счётчики для сводки панели. Персистятся с State.
type Counters struct {
	Registrations  int `json:"registrations_total"`
	MasterSwitches int `json:"master_switches_total"`
	GPBlocked      int `json:"gp_blocked_total"`
	DNSUpdates     int `json:"dns_updates_total"`
	DNSErrors      int `json:"dns_errors_total"`
	HealthReports  int `json:"health_reports_total"`
	Heartbeats     int `json:"heartbeats_total"`
	// MasterTTLRotations — сколько раз мастерство сменилось
	// принудительно по истечении master_ttl_minutes (не по болезни).
	MasterTTLRotations int `json:"master_ttl_rotations_total"`
	// NodesTerminated — терминально завершённых нод (все причины).
	NodesTerminated int `json:"nodes_terminated_total"`
	// Действия СРМД (создание/сворачивание/разворачивание доменов).
	SRMDCreated  int `json:"srmd_created_total"`
	SRMDFolded   int `json:"srmd_folded_total"`
	SRMDUnfolded int `json:"srmd_unfolded_total"`
}

// addEventLocked добавляет событие в журнал и помечает состояние грязным
// (сброс на диск делает persistStateLocked или eventPersistLoop).
// ВЫЗЫВАТЬ СТРОГО под r.mu (write-lock).
func (r *Registry) addEventLocked(ev Event) {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	r.state.Events = append(r.state.Events, ev)
	r.cfgMu.RLock()
	max := r.cfg.Panel.EventsMax
	r.cfgMu.RUnlock()
	if max > 0 && len(r.state.Events) > max {
		trimmed := make([]Event, max)
		copy(trimmed, r.state.Events[len(r.state.Events)-max:])
		r.state.Events = trimmed
	}
	r.eventsDirty = true
	// Вечная история — зеркалим событие в SQLite (см. db.go).
	// Запись sub-мс и под r.mu укладывается рядом с persistStateLocked.
	if r.db != nil {
		r.db.recordEvent(ev)
	}
}

// eventPersistLoop периодически сбрасывает журнал на диск, чтобы события
// (flap'и, блокировки) переживали рестарт/краш без немедленной записи на
// каждое событие. Критичные точки (мастер, регистрация) персистятся сразу
// через persistStateLocked в местах изменения.
func (r *Registry) eventPersistLoop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		r.mu.Lock()
		if r.eventsDirty {
			r.persistStateLocked()
		}
		r.mu.Unlock()
	}
}

// closeMasterStintLocked фиксирует время ноды в роли мастера (вызывается при
// потере мастерства/удалении ноды). Под write-lock.
func closeMasterStintLocked(c *Candidate, now time.Time) {
	if c.MasterSince.IsZero() {
		return
	}
	c.MasterSeconds += int64(now.Sub(c.MasterSince).Seconds())
	c.MasterSince = time.Time{}
}
