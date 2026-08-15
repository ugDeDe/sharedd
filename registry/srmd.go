package main

// СРМД — Система Распределения и Масштабирования Доменов.
//
// Когда нод много, держать их всех под одним доменом неоптимально: у домена
// один мастер, остальные ноды очереди простаивают. СРМД следит за
// соотношением размера здоровой очереди и числа доменов и держит не больше
// [srmd] max_nodes_per_domain нод на домен:
//
// - доменов НЕ ХВАТАЕТ — при включённом [srmd] enabled создаёт сиротские
// домены с инкрементом от основного: shared.ddproxy.xyz →
// shared1.ddproxy.xyz, shared2.ddproxy.xyz, … — сама дописывает их в
// cloudflare.domains (персистится в registry.toml) и отдаёт в обычную
// селекцию мастеров. По умолчанию создание ВЫКЛЮЧЕНО;
// - доменов СЛИШКОМ (пул сжался) — сворачивает лишние в CNAME на
// оставшиеся, балансируя пользователей: сворачиваемые домены назначаются
// наименее загруженным из оставшихся по хранимой таблице последних
// значений активных клиентов (State.SRMD.DomainClients). Значение
// хранится, даже если живого мастера на домене сейчас нет — балансируем
// по последним известным числам;
// - ноды снова выросли — сначала разворачиваются ранее свёрнутые домены
// (A-запись вернёт обычная селекция: upsertARecord вытесняет CNAME),
// и только потом создаются новые.
//
// Кто выживает при сворачивании: не-созданные СРМД домены (основной и
// добавленные оператором вручную) не сворачиваются НИКОГДА; созданные
// сворачиваются последними по счёту создания. Целевые домены для CNAME —
// только из выживших (цепочек CNAME не бывает).
//
// Свернутый домен остаётся в managed-списке: его DNS-запись — CNAME
// (upsertCNAMERecord), в выборе мастеров он не участвует, а при полном
// удалении домена из конфига его записи вычистит обычный orphan-механизм.
//
// Анти-флап: масштабирование случается только после srmdStableTicks ПОДРЯД
// тиков selectionLoop с устойчивым условием (минута при дефолтном
// selection_interval_ms). Счётчики in-memory — рестарт регистратора их
// обнуляет (осознанно, как ttlOverdue).
//
// Клиенты считаются ТОЛЬКО по общему секрету: ноды суммируют per-user серии
// telemt_user_unique_ips_current лишь по пользователям из shared_proxy.users
// (см. node/healthcheck.go buildMetricsSnapshot) — локальные юзеры ноды в
// числах СРМД не участвуют.

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
)

const (
	defaultSRMDMaxNodesPerDomain = 5
	// srmdStableTicks — сколько подряд тиков селекции условие «доменов не
	// хватает» / «доменов слишком много» должно продержаться до действия.
	srmdStableTicks = 20
	// srmdMaxDomains — предохранительный потолок общего числа managed-доменов.
	srmdMaxDomains = 50
)

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

// srmdDNSRetryDelay — пауза между повторами неудавшейся CNAME-записи СРМД:
// Cloudflare может долго отвечать отказом (плохой токен/зона), долбить его
// каждый тик селекции и спамить журнал dns_error не нужно.
const srmdDNSRetryDelay = time.Minute

// srmdDNSAction — отложенная DNS-запись СРМД (CNAME при сворачивании).
// Копится под r.mu, пишется в selectionLoop вне локов; сбой — retry с
// бэкоффом NextAttempt.
type srmdDNSAction struct {
	Domain      string
	Target      string    // цель CNAME
	NextAttempt time.Time // раньше этого момента не ретраить
}

// resolveSRMDMaxNodes — эффективный лимит нод на домен (0/мусор → дефолт).
func resolveSRMDMaxNodes(v int) int {
	if v <= 0 {
		return defaultSRMDMaxNodesPerDomain
	}
	return v
}

// srmdRequiredDomains — сколько доменов нужно под очередь: ceil, минимум 1.
func srmdRequiredDomains(queueSize, maxPerDomain int) int {
	maxPerDomain = resolveSRMDMaxNodes(maxPerDomain)
	if queueSize <= 0 {
		return 1
	}
	return (queueSize + maxPerDomain - 1) / maxPerDomain
}

// srmdSplitBase — «shared.ddproxy.xyz» → («shared», «.ddproxy.xyz»): схема
// инкремента сиротских доменов. ok=false — имени некуда расти.
func srmdSplitBase(base string) (prefix, suffix string, ok bool) {
	i := strings.Index(base, ".")
	if i <= 0 || i == len(base)-1 {
		return "", "", false
	}
	return base[:i], base[i:], true
}

// srmdBaseDomainLocked — основной домен: явный [srmd] base_domain или первый
// из cloudflare.domains. ВЫЗЫВАТЬ под r.mu (cfg читается под cfgMu).
func (r *Registry) srmdBaseDomainLocked() string {
	r.cfgMu.RLock()
	defer r.cfgMu.RUnlock()
	if d := strings.TrimSpace(r.cfg.SRMD.BaseDomain); d != "" {
		return d
	}
	for _, d := range r.cfg.Cloudflare.Domains {
		if d = strings.TrimSpace(d); d != "" {
			return d
		}
	}
	return ""
}

// srmdNameTakenLocked — имя уже занято (конфиг, назначения, managed-память,
// память созданных СРМД). ВЫЗЫВАТЬ под r.mu.
func (r *Registry) srmdNameTakenLocked(name string) bool {
	r.cfgMu.RLock()
	for _, d := range r.cfg.Cloudflare.Domains {
		if strings.EqualFold(strings.TrimSpace(d), name) {
			r.cfgMu.RUnlock()
			return true
		}
	}
	r.cfgMu.RUnlock()
	for d := range r.state.Assignments {
		if strings.EqualFold(d, name) {
			return true
		}
	}
	for _, d := range r.state.ManagedDomains {
		if strings.EqualFold(d, name) {
			return true
		}
	}
	for _, d := range r.state.SRMD.Created {
		if strings.EqualFold(d, name) {
			return true
		}
	}
	return false
}

// srmdNextNameLocked — первый свободный инкремент: prefix1+suffix, prefix2+…
// ВЫЗЫВАТЬ под r.mu.
func (r *Registry) srmdNextNameLocked(prefix, suffix string) string {
	for i := 1; i < 1000; i++ {
		name := fmt.Sprintf("%s%d%s", prefix, i, suffix)
		if !r.srmdNameTakenLocked(name) {
			return name
		}
	}
	return ""
}

// srmdKeepOrderLocked — managed-домены в порядке «кто выживает»: сначала
// НЕ созданные СРМД (основной и ручные, порядок конфига), затем созданные
// в порядке создания. Домены, выкинутые из конфига вручную, сюда не
// попадают. ВЫЗЫВАТЬ под r.mu.
func (r *Registry) srmdKeepOrderLocked() []string {
	r.cfgMu.RLock()
	raw := append([]string(nil), r.cfg.Cloudflare.Domains...)
	r.cfgMu.RUnlock()

	inCfg := make(map[string]bool, len(raw))
	for _, d := range raw {
		if d = strings.TrimSpace(d); d != "" {
			inCfg[d] = true
		}
	}
	created := make(map[string]bool, len(r.state.SRMD.Created))
	for _, d := range r.state.SRMD.Created {
		created[d] = true
	}
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	add := func(d string) {
		if d == "" || seen[d] {
			return
		}
		seen[d] = true
		out = append(out, d)
	}
	for _, d := range raw {
		d = strings.TrimSpace(d)
		if d != "" && !created[d] {
			add(d)
		}
	}
	for _, d := range r.state.SRMD.Created {
		if inCfg[d] {
			add(d)
		}
	}
	return out
}

// srmdGCLocked — уборка после ручных правок конфига: созданные домены,
// выведенные из managed-списка, забываются в Created; CNAME-статус и числа
// клиентов доменов, которых больше нет в конфиге, стираются. Возвращает
// true, если что-то изменилось. ВЫЗЫВАТЬ под r.mu.
func (r *Registry) srmdGCLocked() bool {
	r.cfgMu.RLock()
	inCfg := make(map[string]bool, len(r.cfg.Cloudflare.Domains))
	for _, d := range r.cfg.Cloudflare.Domains {
		if d = strings.TrimSpace(d); d != "" {
			inCfg[d] = true
		}
	}
	r.cfgMu.RUnlock()

	changed := false
	kept := make([]string, 0, len(r.state.SRMD.Created))
	for _, d := range r.state.SRMD.Created {
		if inCfg[d] {
			kept = append(kept, d)
		} else {
			changed = true
		}
	}
	if changed {
		r.state.SRMD.Created = kept
	}
	for d := range r.state.SRMD.CNames {
		if !inCfg[d] {
			delete(r.state.SRMD.CNames, d)
			changed = true
		}
	}
	for d := range r.state.SRMD.DomainClients {
		if !inCfg[d] {
			delete(r.state.SRMD.DomainClients, d)
			changed = true
		}
	}
	return changed
}

// srmdRebalanceLocked — шаг СРМД внутри evaluateAssignments: обновляет
// таблицу клиентов по доменам и масштабирует число доменов под очередь.
// queue — уже построенный список fully-healthy нод. Возвращает true, если
// состояние изменилось (нужен персист). ВЫЗЫВАТЬ под r.mu (write; порядок
// локов mu→cfgMu соблюдён).
func (r *Registry) srmdRebalanceLocked(now time.Time, queue []*Candidate) bool {
	if r.state.SRMD.DomainClients == nil {
		r.state.SRMD.DomainClients = make(map[string]int)
	}
	if r.state.SRMD.CNames == nil {
		r.state.SRMD.CNames = make(map[string]string)
	}
	changed := r.srmdGCLocked()

	// 1. таблица «домен | активные пользователи»: последние значения — с
	// живых мастеров (нода считает их только по общему секрету). Мастер
	// пропал/не присылает метрику — держим последнее известное число.
	for d, id := range r.state.Assignments {
		c := r.state.Candidates[id]
		if c == nil {
			continue
		}
		if v, ok := c.MetricsSnapshot[uniqueIPsMetric]; ok {
			if r.state.SRMD.DomainClients[d] != int(v) {
				r.state.SRMD.DomainClients[d] = int(v)
				changed = true
			}
		}
	}

	// 2. снимок настроек (горячие — под cfgMu)
	r.cfgMu.RLock()
	enabled := r.cfg.SRMD.Enabled != nil && *r.cfg.SRMD.Enabled
	maxN := resolveSRMDMaxNodes(r.cfg.SRMD.MaxNodesPerDomain)
	r.cfgMu.RUnlock()

	order := r.srmdKeepOrderLocked()
	if len(order) == 0 {
		r.srmdExpandTicks, r.srmdFoldTicks = 0, 0
		return changed
	}
	active := make([]string, 0, len(order))
	for _, d := range order {
		if r.state.SRMD.CNames[d] == "" {
			active = append(active, d)
		}
	}
	required := srmdRequiredDomains(len(queue), maxN)

	switch {
	case enabled && len(active) < required:
		// доменов не хватает: разворачиваем свёрнутые / создаём новые
		r.srmdFoldTicks = 0
		r.srmdExpandTicks++
		if r.srmdExpandTicks < srmdStableTicks {
			return changed
		}
		r.srmdExpandTicks = 0
		if c := r.srmdExpandLocked(now, order, len(active), required, len(queue), maxN); c {
			changed = true
		}
	case enabled && len(active) > required:
		// доменов слишком много (пул сжался): сворачиваем лишние в CNAME
		r.srmdExpandTicks = 0
		r.srmdFoldTicks++
		if r.srmdFoldTicks < srmdStableTicks {
			return changed
		}
		r.srmdFoldTicks = 0
		if c := r.srmdFoldLocked(now, active, required); c {
			changed = true
		}
	default:
		r.srmdExpandTicks, r.srmdFoldTicks = 0, 0
	}
	return changed
}

// srmdExpandLocked — дотянуть число АКТИВНЫХ доменов до required: сначала
// разворачиваются ранее свёрнутые (в порядке создания), затем создаются
// новые инкременты основного домена. ВЫЗЫВАТЬ под r.mu.
func (r *Registry) srmdExpandLocked(now time.Time, order []string, activeCount, required, queueSize, maxN int) bool {
	changed := false
	need := required - activeCount

	// 1. свободные свёрнутые домены — раньше новых созданий
	for _, d := range order {
		if need <= 0 {
			break
		}
		target, folded := r.state.SRMD.CNames[d]
		if !folded {
			continue
		}
		delete(r.state.SRMD.CNames, d)
		r.state.Counters.SRMDUnfolded++
		r.addEventLocked(Event{
			Type: EventSRMDDomainUnfolded, Domain: d,
			Detail: fmt.Sprintf("СРМД: очередь выросла (%d нод, макс %d/домен) — домен возвращён в ротацию, был CNAME -> %s",
				queueSize, maxN, target),
		})
		log.Printf("srmd: domain %s unfolded (was CNAME -> %s) — queue %d, required %d", d, target, queueSize, required)
		changed = true
		need--
	}

	// 2. новых доменов: инкременты от основного
	base := r.srmdBaseDomainLocked()
	prefix, suffix, nameOK := srmdSplitBase(base)
	for need > 0 {
		r.cfgMu.RLock()
		total := len(r.cfg.Cloudflare.Domains)
		r.cfgMu.RUnlock()
		if total >= srmdMaxDomains {
			log.Printf("srmd: domain cap reached (%d) — не создаём, очередь %d ждёт свободных доменов", srmdMaxDomains, queueSize)
			break
		}
		if !nameOK {
			log.Printf("srmd: base domain %q не даёт схемы инкремента — новые домены не создаются", base)
			break
		}
		name := r.srmdNextNameLocked(prefix, suffix)
		if name == "" {
			break
		}
		// дописываем домен в управление: конфиг (под cfgMu, mu уже держим) +
		// память СРМД. Селекция прочитает расширенный список в этом же тике.
		r.cfgMu.Lock()
		r.cfg.Cloudflare.Domains = append(r.cfg.Cloudflare.Domains, name)
		perr := r.persistConfigLocked()
		r.cfgMu.Unlock()
		r.state.SRMD.Created = append(r.state.SRMD.Created, name)
		r.state.Counters.SRMDCreated++
		detail := fmt.Sprintf("СРМД: очередь %d здоровых нод при максимуме %d нод/домен — создан сиротский домен",
			queueSize, maxN)
		if perr != nil {
			detail += "; ВНИМАНИЕ: конфиг не персистился: " + perr.Error()
		}
		r.addEventLocked(Event{Type: EventSRMDDomainCreated, Domain: name, Detail: detail})
		log.Printf("srmd: created domain %s (queue %d, max %d nodes/domain, persisted=%v)", name, queueSize, maxN, perr == nil)
		changed = true
		need--
	}
	return changed
}

// srmdFoldLocked — свернуть лишние АКТИВНЫЕ домены в CNAME на выживших.
// active — активные домены в порядке выживания (основные, затем созданные).
// Целевые: первые required активных, но НЕ меньше числа не-созданных (основные
// и ручные не сворачиваются никогда). Балансировка жадная: более нагруженные
// сворачиваемые распределяются первыми, каждый — на наименее загруженного из
// выживших на данный момент. ВЫЗЫВАТЬ под r.mu.
func (r *Registry) srmdFoldLocked(now time.Time, active []string, required int) bool {
	created := make(map[string]bool, len(r.state.SRMD.Created))
	for _, d := range r.state.SRMD.Created {
		created[d] = true
	}
	nonCreated := 0
	for _, d := range active {
		if !created[d] {
			nonCreated++
		}
	}
	keep := required
	if keep < nonCreated {
		keep = nonCreated // основные/ручные домены не сворачиваются
	}
	if keep >= len(active) {
		return false
	}
	survivors := append([]string(nil), active[:keep]...)
	foldables := append([]string(nil), active[keep:]...)

	// крупные сворачиваемые домены распределяем первыми
	sort.SliceStable(foldables, func(i, j int) bool {
		return r.state.SRMD.DomainClients[foldables[i]] > r.state.SRMD.DomainClients[foldables[j]]
	})
	load := make(map[string]int, len(survivors))
	for _, d := range survivors {
		load[d] = r.state.SRMD.DomainClients[d]
	}

	changed := false
	for _, d := range foldables {
		target := survivors[0]
		for _, s := range survivors[1:] {
			if load[s] < load[target] {
				target = s
			}
		}
		clients := r.state.SRMD.DomainClients[d]
		load[target] += clients
		r.state.SRMD.CNames[d] = target
		r.state.SRMD.DomainClients[target] = load[target]
		// домен выходит из ротации: мастер сдаёт назначение
		if holderID := r.state.Assignments[d]; holderID != "" {
			delete(r.state.Assignments, d)
			delete(r.state.AssignmentsSince, d)
			delete(r.ttlOverdue, d)
		}
		r.state.Counters.SRMDFolded++
		r.addEventLocked(Event{
			Type: EventSRMDDomainFolded, Domain: d,
			Detail: fmt.Sprintf("СРМД: доменов больше, чем нужно пулу — CNAME -> %s (баланс клиентов: %d + %d = %d)",
				target, load[target]-clients, clients, load[target]),
		})
		log.Printf("srmd: folded domain %s -> CNAME %s (clients %d+%d=%d)", d, target, load[target]-clients, clients, load[target])
		// CNAME запишет selectionLoop вне локов (flushSRMDDNS)
		r.srmdPending = append(r.srmdPending, srmdDNSAction{Domain: d, Target: target})
		changed = true
	}
	return changed
}

// flushSRMDDNS — пишет накопленные СРМД CNAME (вне локов: Cloudflare —
// сеть). Сбой вернёт действие в очередь с бэкоффом srmdDNSRetryDelay —
// retry не чаще раза в минуту, журнал не спамится.
func (r *Registry) flushSRMDDNS() {
	now := time.Now()
	r.mu.Lock()
	if len(r.srmdPending) == 0 {
		r.mu.Unlock()
		return
	}
	var due []srmdDNSAction
	rest := make([]srmdDNSAction, 0, len(r.srmdPending))
	for _, a := range r.srmdPending {
		if now.Before(a.NextAttempt) {
			rest = append(rest, a) // бэкофф ещё не вышел
		} else {
			due = append(due, a)
		}
	}
	r.srmdPending = rest
	r.mu.Unlock()

	for _, a := range due {
		err := r.upsertCNAMERecord(a.Domain, a.Target)
		r.mu.Lock()
		if err != nil {
			r.state.Counters.DNSErrors++
			r.addEventLocked(Event{Type: EventDNSError, Domain: a.Domain, Detail: "srmd cname: " + err.Error()})
			a.NextAttempt = time.Now().Add(srmdDNSRetryDelay)
			r.srmdPending = append(r.srmdPending, a)
		} else {
			r.state.Counters.DNSUpdates++
			r.addEventLocked(Event{Type: EventDNSUpdated, Domain: a.Domain, Detail: "CNAME -> " + a.Target + " (СРМД)"})
		}
		r.persistStateLocked()
		r.mu.Unlock()
	}
}

// ── панель: вкладка «СРМД» ─────────────────────────────────────

// panelSRMDDomain — строка таблицы «домен | активные пользователи».
type panelSRMDDomain struct {
	Domain  string `json:"domain"`
	Clients *int   `json:"clients"`           // nil = никогда не измерялось
	Fresh   bool   `json:"fresh"`             // значение снято с живого мастера сейчас
	CName   string `json:"cname,omitempty"`   // свёрнут: цель CNAME
	NodeID  string `json:"node_id,omitempty"` // текущий мастер (если есть)
	IP      string `json:"ip,omitempty"`
	Base    bool   `json:"base,omitempty"`    // основной домен
	Created bool   `json:"created,omitempty"` // создан СРМД
}

// panelSRMD — блок СРМД для /panel/api/overview.
type panelSRMD struct {
	Enabled           bool              `json:"enabled"`
	BaseDomain        string            `json:"base_domain"`
	MaxNodesPerDomain int               `json:"max_nodes_per_domain"`
	QueueSize         int               `json:"queue_size"`       // здоровая очередь
	RequiredDomains   int               `json:"required_domains"` // нужно доменов под очередь
	TotalDomains      int               `json:"total_domains"`    // managed всего
	FoldedDomains     int               `json:"folded_domains"`   // из них свёрнуто в CNAME
	Alert             string            `json:"alert,omitempty"`  // создание выключено, а нод слишком много
	Domains           []panelSRMDDomain `json:"domains"`
}

// buildPanelSRMDLocked — снимок СРМД для панели. ВЫЗЫВАТЬ под r.mu
// (минимум RLock).
func (r *Registry) buildPanelSRMDLocked(queueSize int, enabled bool, baseCfg string, maxN int) *panelSRMD {
	base := baseCfg
	if base == "" {
		r.cfgMu.RLock()
		for _, d := range r.cfg.Cloudflare.Domains {
			if d = strings.TrimSpace(d); d != "" {
				base = d
				break
			}
		}
		r.cfgMu.RUnlock()
	}
	order := r.srmdKeepOrderLocked()
	created := make(map[string]bool, len(r.state.SRMD.Created))
	for _, d := range r.state.SRMD.Created {
		created[d] = true
	}
	s := &panelSRMD{
		Enabled:           enabled,
		BaseDomain:        base,
		MaxNodesPerDomain: maxN,
		QueueSize:         queueSize,
		RequiredDomains:   srmdRequiredDomains(queueSize, maxN),
		TotalDomains:      len(order),
		Domains:           make([]panelSRMDDomain, 0, len(order)),
	}
	activeCount := 0
	for _, d := range order {
		row := panelSRMDDomain{Domain: d, Base: d == base, Created: created[d], CName: r.state.SRMD.CNames[d]}
		if cl, ok := r.state.SRMD.DomainClients[d]; ok {
			v := cl
			row.Clients = &v
		}
		if row.CName == "" {
			activeCount++
			if id := r.state.Assignments[d]; id != "" {
				row.NodeID = id
				if c := r.state.Candidates[id]; c != nil {
					row.IP = c.IP
					if _, ok := c.MetricsSnapshot[uniqueIPsMetric]; ok {
						row.Fresh = true
					}
				}
			}
		} else {
			s.FoldedDomains++
		}
		s.Domains = append(s.Domains, row)
	}
	if !enabled && queueSize > activeCount*maxN {
		s.Alert = fmt.Sprintf("СРМД: нод слишком много в очереди — %d здоровых нод при %d активных доменах × %d нод/домен. Включите «Разрешить СРМД создавать новые домены» (панель → СРМД) или добавьте домены вручную.",
			queueSize, activeCount, maxN)
	}
	return s
}
