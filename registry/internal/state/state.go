// Package state — персистентное состояние регистратора: кандидаты пула,
// назначения мастеров, журнал событий, блок-листы и СРМД-таблицы, плюс
// загрузка/сохранение JSON-state-файла атомарной записью.
//
// Пакет намеренно без зависимостей от остальных частей сервера: только
// stdlib. Остальные пакеты (server, machineapi, webui, history) импортируют
// его, но не наоборот.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ── журнал событий ──────────────────────────────────────────────

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
	// GP-карантин и терминальное завершение нод.
	EventNodeQuarantined     = "node_quarantined"     // всё зелёное кроме GP — отдельная таблица, ждёт вердикта
	EventQuarantineRecovered = "quarantine_recovered" // GP в карантине позеленел — нода возвращается нормально
	EventNodeTerminated      = "node_terminated"      // финал: бан (ip_ban/dead), нода мертва, регистраций нет
	EventBanLifted           = "ban_lifted"           // ip_ban снят: служба перезапущена с нового IP (история бана сохраняется)
	EventIPBlocked           = "ip_blocked"           // нода сменила ip в карантине — старый ip записан как бан
	// СРМД — масштабирование пула доменов.
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

// ── история точек проверок ───────────────────────────────────────

type TCPPoint struct {
	At time.Time `json:"at"`
	OK bool      `json:"ok"`
}

type GPPoint struct {
	At          time.Time `json:"at"`
	OK          bool      `json:"ok"`
	Ratio       float64   `json:"ratio"`
	ProbesOK    int       `json:"probes_ok"`
	ProbesTotal int       `json:"probes_total"`
}

type ReportPoint struct {
	At        time.Time `json:"at"`
	MetricsOK bool      `json:"metrics_ok"`
	Clients   int       `json:"clients"` // -1 = метрики нет (старый агент / user_enabled=false)
	Writers   int       `json:"writers"`
}

type GPProbeLine struct {
	Country  string `json:"country"`
	City     string `json:"city,omitempty"`
	Network  string `json:"network,omitempty"`
	ASN      int    `json:"asn,omitempty"`
	OK       bool   `json:"ok"`
	Status   string `json:"status"`
	HTTPCode int    `json:"http_code,omitempty"`
}

type GPDetail struct {
	At            time.Time     `json:"at"`
	MeasurementID string        `json:"measurement_id"`
	OK            bool          `json:"ok"`
	Ratio         float64       `json:"ratio"`
	ProbesOK      int           `json:"probes_ok"`
	ProbesTotal   int           `json:"probes_total"`
	Probes        []GPProbeLine `json:"probes,omitempty"`
}

// PushRing — append в кольцевой буфер с лимитом длины (старшие точки вытесняются).
func PushRing[T any](s []T, v T, limit int) []T {
	s = append(s, v)
	if n := len(s) - limit; n > 0 {
		copy(s, s[n:])
		s = s[:limit]
	}
	return s
}

// ── причины банов ───────────────────────────────────────────────

const (
	BanReasonIPBan = "ip_ban" // блокировка по IP (финал GP-карантина)
	BanReasonDead  = "dead"   // регистратор не достучался до порта/метрик >N мин
)

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

// PruneTombstone — карантинная запись по вычищенной рипером ноде. Персистится
// в state: рестарт регистратора карантин не отменяет.
type PruneTombstone struct {
	NodeID      string    `json:"node_id"`
	IP          string    `json:"ip,omitempty"`
	Strikes     int       `json:"strikes"` // серия prune подряд (→ длина карантина)
	LastPruned  time.Time `json:"last_pruned"`
	BannedUntil time.Time `json:"banned_until"` // до этого момента /register отклоняется 429
}

type DNSOperation struct {
	DesiredType   string    `json:"desired_type,omitempty"`
	DesiredTarget string    `json:"desired_target,omitempty"`
	DesiredNode   string    `json:"desired_node,omitempty"`
	AppliedType   string    `json:"applied_type,omitempty"`
	AppliedTarget string    `json:"applied_target,omitempty"`
	Attempts      int       `json:"attempts,omitempty"`
	NextAttempt   time.Time `json:"next_attempt,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
	LastSuccess   time.Time `json:"last_success,omitempty"`
	Generation    uint64    `json:"generation,omitempty"`
}

// Drifted — желаемая DNS-операция ещё не применена (или применено другое).
func (op *DNSOperation) Drifted() bool {
	return op.DesiredType != op.AppliedType || op.DesiredTarget != op.AppliedTarget
}

// SRMDState — персистентная часть СРМД (внутри State).
type SRMDState struct {
	// DomainClients — ПОСЛЕДНИЕ известные активные клиенты (уникальные IP по
	// общему секрету) на домене: снимается с живого мастера, а при его
	// потере значение НЕ стирается — на этих числах стоит балансировка
	// сворачивания и таблица панели.
	DomainClients map[string]int `json:"domain_clients,omitempty"`
	// CNames — свёрнутые домены: domain → цель CNAME. Свёрнутый домен не
	// участвует в выборе мастеров; его DNS-запись — CNAME на цель.
	CNames map[string]string `json:"cnames,omitempty"`
	// Created — созданные СРМД домены в ПОРЯДКЕ СОЗДАНИЯ (инкременты
	// shared1, shared2, …). Порядок определяет очередь на сворачивание
	// (последние созданные сворачиваются первыми) и на разворачивание
	// (ранние разворачиваются первыми).
	Created []string `json:"created,omitempty"`
}

// ── кандидат ────────────────────────────────────────────────────

type Candidate struct {
	NodeID          string    `json:"node_id"`
	IP              string    `json:"ip"`
	RegisteredAt    time.Time `json:"registered_at"`
	LastHeartbeat   time.Time `json:"last_heartbeat"`
	Healthy         bool      `json:"healthy"`
	ConsecutiveFail int       `json:"-"`
	ConsecutiveOK   int       `json:"-"`

	// Очередь мастерства считается по НЕПРЕРЫВНОМУ здоровью, а не по RegisteredAt:
	// QueuedAt — момент последнего входа в fully-healthy состояние; пока нода
	// здорова, позиция не меняется. Выпала из fully-healthy по любой причине
	// (telemt умер, globalping упал, отчёты протухли, TCP не отвечает) — при
	// возврате встаёт в конец очереди.
	FullyHealthy bool      `json:"fully_healthy"`
	QueuedAt     time.Time `json:"queued_at"`

	// UnhealthySince — момент последнего выхода из очереди здоровых
	// (= старт непрерывного «всё красное» эпизода). Ноль — нода в очереди.
	// Используется рипером prune: непрерывно вне очереди дольше
	// PruneUnhealthyTTL → удаление из пула. Персистится в state.
	UnhealthySince time.Time `json:"unhealthy_since,omitempty"`

	// DeadBothSince — старт непрерывного окна класса dead:
	// TCP-защёлка красная И метрик нет (отчёты протухли или красные).
	// Достиг TerminateDeadTTL → терминальное завершение.
	DeadBothSince time.Time `json:"dead_both_since,omitempty"`
	// Quarantine — нода в GP-карантине («всё зелёное, кроме
	// globalping»): панель показывает отдельной таблицей, prune-рипер не
	// трогает; вердикт — счётчик попыток → бан по IP или восстановление.
	Quarantine *QuarantineState `json:"quarantine,omitempty"`

	GlobalpingOK            bool      `json:"globalping_ok"`
	GlobalpingMeasurementID string    `json:"globalping_measurement_id"`
	GlobalpingVerifiedRatio float64   `json:"globalping_verified_ratio"`
	LastGlobalpingAt        time.Time `json:"last_globalping_at,omitempty"`
	LastGlobalpingRequestAt time.Time `json:"-"`
	MetricsOK               bool      `json:"metrics_ok"` // сырой вердикт ПОСЛЕДНЕГО отчёта (мигает от любого чиха)
	// MetricsHealthy — защёлка metrics-здоровья по fail/recover-
	// порогам, полный аналог TCP-защёлки Healthy: в fully-healthy входит
	// ИМЕННО она, а не MetricsOK последнего отчёта.
	MetricsHealthy    bool               `json:"metrics_healthy"`
	MetricsFailStreak int                `json:"-"`
	MetricsOKStreak   int                `json:"-"`
	MetricsSnapshot   map[string]float64 `json:"metrics_snapshot,omitempty"`
	LastReportAt      time.Time          `json:"last_report_at"`
	ReportError       string             `json:"report_error,omitempty"`
	// Generation changes whenever registration refreshes the candidate. Report
	// verification may perform a slow network fetch and must not update a
	// re-registered or replaced candidate afterwards.
	Generation            uint64    `json:"generation,omitempty"`
	LastAcceptedCheckedAt time.Time `json:"last_accepted_checked_at,omitempty"`
	LastMetricsCheckedAt  time.Time `json:"last_metrics_checked_at,omitempty"`
	LastGPCheckedAt       time.Time `json:"last_gp_checked_at,omitempty"`
	UsedMeasurementIDs    []string  `json:"used_measurement_ids,omitempty"`

	Port int `json:"port"`
	// nil keeps legacy persisted candidates compatible until their next report.
	PortCompatible *bool `json:"port_compatible,omitempty"`

	// NodeType: classic/mtproxyl/meko — тип менеджера прокси на ноде,
	// информационный бейдж в панели.
	NodeType string `json:"node_type,omitempty"`

	// Накопительная статистика ноды (для панели/доступности).
	HeartbeatsTotal int `json:"heartbeats_total"`
	ReportsTotal    int `json:"reports_total"`
	ReportsOK       int `json:"reports_ok"`
	GPChecksTotal   int `json:"gp_checks_total"` // сколько раз независимо проверяли Globalping
	GPChecksOK      int `json:"gp_checks_ok"`

	// Учёт времени в роли мастера: MasterSince — начало текущего stint'а,
	// MasterSeconds — сумма закрытых stint'ов (сек).
	MasterStints  int       `json:"master_stints"`
	MasterSeconds int64     `json:"master_seconds"`
	MasterSince   time.Time `json:"master_since,omitempty"`

	// История для детальной страницы ноды: кольцевые буферы точек,
	// персистятся вместе с State. GPLast — деталь последнего measurement'а
	// Globalping (результаты по площадкам).
	TCPHist    []TCPPoint    `json:"tcp_hist,omitempty"`
	GPHist     []GPPoint     `json:"gp_hist,omitempty"`
	ReportHist []ReportPoint `json:"report_hist,omitempty"`
	GPLast     *GPDetail     `json:"gp_last,omitempty"`
}

// HadMasterTime — успела ли нода поработать мастером: есть/был stint
// (закрытые секунды либо открытый MasterSince). Это условие записи бана
// в статистику: бан ноды, не задевшей пользователей, счётчики не двигает.
func (c *Candidate) HadMasterTime(now time.Time) bool {
	return c.MasterStints > 0 || c.MasterTimeSec(now) > 0
}

// IsFullyHealthy — все сигналы зелёные И отчёт свежий.
func (c *Candidate) IsFullyHealthy(freshnessTTL time.Duration) bool {
	if !c.Healthy {
		return false
	}
	// Вместо сырого MetricsOK последнего отчёта — защёлка
	// MetricsHealthy (fail/recover-пороги). GlobalpingOK остаётся
	// мгновенным: это НЕЗАВИСИМАЯ верификация регистратора, нода её
	// подделать не может, а подтверждённо заблокированный мастер —
	// мёртвый груз для домена, задержка ротации тут только вредит.
	if !c.GlobalpingOK || !c.MetricsHealthy {
		return false
	}
	if c.PortCompatible != nil && !*c.PortCompatible {
		return false
	}
	if c.LastReportAt.IsZero() || time.Since(c.LastReportAt) > freshnessTTL {
		return false
	}
	return true
}

// UnhealthyReason — человекочитаемая первопричина того, что нода НЕ fully
// healthy. Используется в журнале событий (queue_left) и панели.
func (c *Candidate) UnhealthyReason(freshnessTTL time.Duration) string {
	switch {
	case c.PortCompatible != nil && !*c.PortCompatible:
		return fmt.Sprintf("proxy port %d differs from registry shared port", c.Port)
	case c.Quarantine != nil:
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
		// Сюда попадаем только после серии плохих отчётов (защёлка)
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

// ── корневое состояние ──────────────────────────────────────────

type State struct {
	Candidates    map[string]*Candidate    `json:"candidates"`
	DNSOperations map[string]*DNSOperation `json:"dns_operations,omitempty"`
	// Assignments — per-domain мастера, domain → node_id. Каждый managed-
	// домен держит свою ноду; при дефиците нод мастера забирают «сиротские»
	// домены (fill-empty), при появлении свободной ноды сирота отдаётся ей.
	Assignments map[string]string `json:"assignments,omitempty"`
	// AssignmentsSince — domain → момент текущего назначения. База TTL
	// мастерства ([rotation] master_ttl_minutes); персистится — отсчёт не
	// сбрасывается рестартом регистратора.
	AssignmentsSince map[string]time.Time `json:"assignments_since,omitempty"`
	Events           []Event              `json:"events,omitempty"`
	Counters         Counters             `json:"counters"`
	// PruneStrikes — карантин вычищенных рипером нод (node_id → запись).
	PruneStrikes map[string]*PruneTombstone `json:"prune_strikes,omitempty"`
	// ManagedDomains — все домены, которыми регистратор когда-либо
	// управлял. Только для них разрешена зачистка DNS при удалении из
	// конфига — чужие записи зоны сюда не попадают и не трогаются никогда.
	ManagedDomains []string `json:"managed_domains,omitempty"`
	// Terminated — оперативный блок-лист убитых нод (node_id → запись).
	// Вечная история всех банов — в SQLite (bans), сюда она не нужна.
	Terminated map[string]*TerminatedRecord `json:"terminated,omitempty"`
	// SRMD — Система Распределения и Масштабирования Доменов.
	SRMD SRMDState `json:"srmd,omitempty"`
}

// ── файл состояния ──────────────────────────────────────────────

// AtomicWriteFile — запись через temp-файл в том же каталоге + rename,
// с fsync файла и каталога.
func AtomicWriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	ok = true
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// Save — маршалит состояние и атомарно пишет его в path (0600).
func Save(path string, st *State) error {
	data, err := json.MarshalIndent(st, "", " ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	return AtomicWriteFile(path, data, 0600)
}

// Load читает state-файл и нормализует его: отсутствующие map'ы создаются,
// null-записи кандидатов выживают, legacy-state без metrics-защёлки получает
// защёлку из последнего вердикта. Файла нет — (nil, os.ErrNotExist).
func Load(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse state file: %w", err)
	}
	if st.Candidates == nil {
		st.Candidates = make(map[string]*Candidate)
	}
	if st.Assignments == nil {
		st.Assignments = make(map[string]string)
	}
	if st.AssignmentsSince == nil {
		st.AssignmentsSince = make(map[string]time.Time)
	}
	if st.DNSOperations == nil {
		st.DNSOperations = make(map[string]*DNSOperation)
	}
	if st.Terminated == nil {
		st.Terminated = make(map[string]*TerminatedRecord)
	}
	// миграция: metrics-защёлки (MetricsHealthy) в старых state нет —
	// после апгрейда нельзя ронять всё здоровое: переносим текущее MetricsOK
	// (вердикт последнего отчёта до выключения) в защёлку.
	for _, c := range st.Candidates {
		// Не доверяем state-файлу: старый/частично записанный JSON может
		// содержать null в map candidates.
		if c == nil {
			continue
		}
		if c.MetricsOK && !c.MetricsHealthy {
			c.MetricsHealthy = true
		}
	}
	return &st, nil
}
