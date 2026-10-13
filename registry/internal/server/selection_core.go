package server

// Чистое ядро селекции мастеров: функции принимают явные входы (очередь,
// назначения, счётчики) и не знают про Registry, локи, DNS и персистенс.
// Оркестратор — evaluateAssignments (в run_main_impl.go): собирает view из
// state, гоняет проходы, сбрасывает sink в журнал/счётчики/DNS.
//
// Мульти-мастер ([selection] masters_per_domain = M): каждый домен держит
// N слотов; слот = индекс в State.Assignments[domain]. Разные слоты одного
// домена обязаны указывать на РАЗНЫЕ ноды с РАЗНЫМИ IP (DNS round-robin).
// M=1 — прежнее поведение байт-в-байт (один слот = старое значение).
//
// Инварианты (см. также комментарий к evaluateAssignments):
// - очередь по непрерывному здоровью, позиция = QueuedAt;
// - живой держатель слота не трогается вне принудительной TTL-ротации;
// - пустой очереди назначений не снимает (записи остаются на последних
//   мастера́х);
// - детерминизм: сортировка очереди и доменов полная (тай-брейки по
//   RegisteredAt/NodeID), обход доменов лексикографический;
// - TTL считается ПО СЛОТУ (AssignmentsSince[domain][i]), ротация заменяет
//   один слот за тик;
// - len(AssignmentsSince[d]) == len(Assignments[d]) поддерживается хелперами.

import (
	"fmt"
	"log"
	"slices"
	"sort"
	"strings"
	"time"

	"sharedd/registry/internal/state"
)

// selectionSink копит побочные эффекты проходов: события, смены мастеров,
// счётчики. Оркестратор сбрасывает его под локом.
type selectionSink struct {
	events       []state.Event
	changes      []domainChange
	switches     int
	ttlRotations int
}

func (s *selectionSink) masterLost(nodeID, ip, domain, detail string) {
	s.events = append(s.events, state.Event{
		Type: state.EventMasterLost, NodeID: nodeID, IP: ip, Domain: domain, Detail: detail,
	})
}

func (s *selectionSink) masterElected(nodeID, ip, domain, detail string) {
	s.events = append(s.events, state.Event{
		Type: state.EventMasterElected, NodeID: nodeID, IP: ip, Domain: domain, Detail: detail,
	})
}

// selectionView — рабочее состояние одного тика селекции. Map'ы Assignments/
// AssignmentsSince/TTLOverdue/All — ССЫЛКИ на живые структуры state (функции
// мутируют их напрямую).
type selectionView struct {
	queue   []*Candidate          // здоровые, отсортированы по старшинству в очереди
	healthy map[string]*Candidate // nodeID → кандидат (только queue)
	all     map[string]*Candidate // nodeID → кандидат (весь пул, для IP держателей)
	holds   map[string][]string   // nodeID → домены держателя

	Assignments      map[string]state.AssignmentSlots
	AssignmentsSince map[string]state.SinceSlots
	TTLOverdue       map[string]bool
	Masters          int // masters_per_domain (1..8)
}

// positionOf — 1-based позиция кандидата в очереди (0 — нет в очереди).
func (st *selectionView) positionOf(c *Candidate) int {
	for i, x := range st.queue {
		if x == c {
			return i + 1
		}
	}
	return 0
}

// toTail — отправить ноду в конец очереди (после сдачи домена по TTL):
// и персистентный QueuedAt, и живой порядок для остатка тика.
func (st *selectionView) toTail(nodeID string, now time.Time) {
	if holder := st.all[nodeID]; holder != nil {
		holder.QueuedAt = now
	}
	for i, x := range st.queue {
		if x.NodeID == nodeID {
			copy(st.queue[i:], st.queue[i+1:])
			st.queue[len(st.queue)-1] = x
			break
		}
	}
}

// setSince — AssignmentsSince[domain][i] = t с выравниванием длины списка.
func (st *selectionView) setSince(domain string, i int, t time.Time) {
	s := st.AssignmentsSince[domain]
	for len(s) <= i {
		s = append(s, time.Time{})
	}
	s[i] = t
	st.AssignmentsSince[domain] = s
}

// slotLabel — «slot i/N» для логов/деталей событий; при M=1 метки нет
// (события остаются в точности прежними).
func (st *selectionView) slotLabel(i int) string {
	if st.Masters <= 1 {
		return ""
	}
	return fmt.Sprintf("slot %d/%d", i+1, st.Masters)
}

func (st *selectionView) slotDetail(i int) string {
	if l := st.slotLabel(i); l != "" {
		return ", " + l
	}
	return ""
}

// pickSlotCandidate — наименее загруженный из очереди для слота домена d:
// раньше в очереди при равенстве загрузки; исключаются exclude (держатель,
// которого нельзя выбирать — TTL-ротация), ноды, уже держащие d, и ноды
// с IP, совпадающим с другими держателями d (разные IP обязательны).
// Для пустого d — простой выбор наименее загруженного.
func pickSlotCandidate(st *selectionView, d, exclude string) *Candidate {
	var otherIPs map[string]bool
	if d != "" {
		otherIPs = make(map[string]bool, len(st.Assignments[d]))
		for _, id := range st.Assignments[d] {
			if id == exclude {
				continue // заменяемый слот — его IP не в счёт
			}
			if c := st.all[id]; c != nil && c.IP != "" {
				otherIPs[c.IP] = true
			}
		}
	}
	var best *Candidate
	for _, c := range st.queue {
		if c.NodeID == exclude {
			continue
		}
		if d != "" && slices.Contains(st.holds[c.NodeID], d) {
			continue // уже держит этот домен
		}
		if otherIPs[c.IP] {
			continue // IP занят другим слотом домена
		}
		if best == nil || len(st.holds[c.NodeID]) < len(st.holds[best.NodeID]) {
			best = c
		}
	}
	return best
}

// sortQueue — очередь по непрерывному здоровью: старый QueuedAt раньше;
// тай-брейки RegisteredAt, затем NodeID (полный порядок = воспроизводимость).
func sortQueue(list []*Candidate) {
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if !a.QueuedAt.Equal(b.QueuedAt) {
			return a.QueuedAt.Before(b.QueuedAt)
		}
		if !a.RegisteredAt.Equal(b.RegisteredAt) {
			return a.RegisteredAt.Before(b.RegisteredAt)
		}
		return a.NodeID < b.NodeID
	})
}

// dropNodeFromSlots — убрать ноду изо всех слотов списка (компактация),
// выравнивая since по тем же индексам. Возвращает (новые слоты, новый since,
// было-ли-что-удалено).
func dropNodeFromSlots(nodes []string, since []time.Time, nodeID string) ([]string, []time.Time, bool) {
	removed := false
	for i := 0; i < len(nodes); i++ {
		if nodes[i] != nodeID {
			continue
		}
		nodes = append(nodes[:i], nodes[i+1:]...)
		if i < len(since) {
			since = append(since[:i], since[i+1:]...)
		}
		removed = true
		i--
	}
	if len(since) > len(nodes) {
		since = since[:len(nodes)]
	}
	return nodes, since, removed
}

// rotateByTTL — pass 0: принудительная ротация по TTL мастерства, ПО СЛОТУ.
// За тик заменяется ОДИН слот домена (остальные не трогаются); здоровый
// держатель с истёкшим лимитом сдаёт слот наименее загруженной ноде (кроме
// себя); замены нет — слот остаётся, OVERDUE логируется раз за эпизод.
func rotateByTTL(st *selectionView, domains []string, ttl time.Duration, now time.Time, s *selectionSink) {
	if ttl <= 0 || len(st.queue) == 0 {
		return
	}
	for _, d := range domains {
		nodes, since := st.Assignments[d], st.AssignmentsSince[d]
		if len(nodes) == 0 {
			continue // домена нет в назначениях — его отдаст pass 1/2
		}
		rotated := false
		var overdueID string
		var overdueAge time.Duration
		var overdueIdx int
		for i, holderID := range nodes {
			if holderID == "" || st.healthy[holderID] == nil {
				delete(st.TTLOverdue, d)
				continue // мёртвые слоты — pass 1
			}
			var start time.Time
			if i < len(since) {
				start = since[i]
			}
			if start.IsZero() {
				// назначение досталось от старой версии — отсчёт отныне
				st.setSince(d, i, now)
				continue
			}
			age := now.Sub(start)
			if age < ttl {
				continue
			}
			if rotated {
				continue // одна ротация слота за тик
			}
			target := pickSlotCandidate(st, d, holderID)
			if target == nil {
				overdueID, overdueAge, overdueIdx = holderID, age, i
				continue
			}
			reason := fmt.Sprintf("master TTL %s expired (was %s) — forced rotation", ttl, age.Round(time.Second))
			holderIP := ""
			if holder := st.all[holderID]; holder != nil {
				holderIP = holder.IP
			}
			s.masterLost(holderID, holderIP, d, reason)
			st.holds[holderID] = dropDomain(st.holds[holderID], d)
			st.holds[target.NodeID] = append(st.holds[target.NodeID], d)
			nodes[i] = target.NodeID
			st.setSince(d, i, now)
			delete(st.TTLOverdue, d)
			s.switches++
			s.ttlRotations++
			st.toTail(holderID, now)
			pos := st.positionOf(target)
			s.masterElected(target.NodeID, target.IP, d,
				fmt.Sprintf("forced rotation (TTL %s), queue #%d, was %q", ttl, pos, holderID)+st.slotDetail(i))
			s.changes = append(s.changes, domainChange{Domain: d, FromID: holderID, ToID: target.NodeID, ToIP: target.IP})
			log.Printf("domain %s: master %q -> %q (%s) %s", d, holderID, target.NodeID, reason, st.slotLabel(i))
			rotated = true
		}
		if !rotated && overdueID != "" && !st.TTLOverdue[d] {
			st.TTLOverdue[d] = true
			if st.Masters > 1 {
				log.Printf("domain %s: master TTL %s exceeded on %s (%s old) but no healthy replacement — keeping %q",
					d, ttl, st.slotLabel(overdueIdx), overdueAge.Round(time.Second), overdueID)
			} else {
				log.Printf("domain %s: master TTL %s exceeded (%s old) but no healthy replacement — keeping %q",
					d, ttl, overdueAge.Round(time.Second), overdueID)
			}
		}
	}
}

// reassignDead — pass 1: переназначение мёртвых/отсутствующих держателей
// СУЩЕСТВУЮЩИХ слотов. Живые держатели других слотов не трогаются;
// заменяется только слот с мёртвым/пустым держателем.
func reassignDead(st *selectionView, domains []string, freshness time.Duration, now time.Time, s *selectionSink) {
	if len(st.queue) == 0 {
		return
	}
	for _, d := range domains {
		nodes := st.Assignments[d]
		if len(nodes) == 0 {
			// домен без назначений отдаётся целиком (прежде: «domain had no
			// master»); дополнительные слоты добьёт pass 2
			target := pickSlotCandidate(st, d, "")
			if target == nil {
				continue
			}
			st.Assignments[d] = state.AssignmentSlots{target.NodeID}
			st.setSince(d, 0, now)
			st.holds[target.NodeID] = append(st.holds[target.NodeID], d)
			delete(st.TTLOverdue, d)
			s.switches++
			pos := st.positionOf(target)
			s.masterElected(target.NodeID, target.IP, d,
				fmt.Sprintf("queue #%d (was %q: domain had no master)", pos, ""))
			s.changes = append(s.changes, domainChange{Domain: d, FromID: "", ToID: target.NodeID, ToIP: target.IP})
			log.Printf("domain %s: master %q -> %q (domain had no master)", d, "", target.NodeID)
			continue
		}
		for i := 0; i < len(nodes); i++ {
			holderID := nodes[i]
			if holderID != "" && st.healthy[holderID] != nil {
				continue // держатель жив
			}
			var reason, holderIP string
			switch holder := st.all[holderID]; {
			case holderID == "":
				reason = "slot had no master"
			case holder == nil:
				reason = "assignee removed from pool"
			default:
				reason = holder.UnhealthyReason(freshness)
				holderIP = holder.IP
			}
			target := pickSlotCandidate(st, d, holderID)
			if target == nil {
				// нет допустимой замены (например, все свободные ноды уже
				// держат домен или заняли его IP) — слот остаётся до след. тика
				continue
			}
			if holderID != "" {
				s.masterLost(holderID, holderIP, d, reason)
				st.holds[holderID] = dropDomain(st.holds[holderID], d)
			}
			nodes[i] = target.NodeID
			st.setSince(d, i, now)
			delete(st.TTLOverdue, d)
			s.switches++
			pos := st.positionOf(target)
			s.masterElected(target.NodeID, target.IP, d,
				fmt.Sprintf("queue #%d (was %q: %s)", pos, holderID, reason)+st.slotDetail(i))
			s.changes = append(s.changes, domainChange{Domain: d, FromID: holderID, ToID: target.NodeID, ToIP: target.IP})
			log.Printf("domain %s: master %q -> %q (%s) %s", d, holderID, target.NodeID, reason, st.slotLabel(i))
		}
	}
}

// fillEmpty — pass 2: (a) свободные слоты (список короче M) заполняются из
// очереди — кандидат не держит домен и имеет IP, отличный от назначенных;
// (b) сироты перетекают к нодам без доменов (старый ребаланс: у самого
// нагруженного держателя забирается лексикографически первый домен целиком;
// при M=1 это предыдущее поведение байт-в-байт). Балансировки 3/1 → 2/2 НЕТ
// сознательно: лишний DNS-черн не оправдан.
func fillEmpty(st *selectionView, now time.Time, s *selectionSink) {
	// (a) свободные слоты — домены в лексикографическом порядке (детерминизм)
	domains := make([]string, 0, len(st.Assignments))
	for d := range st.Assignments {
		domains = append(domains, d)
	}
	sort.Strings(domains)
	for _, d := range domains {
		nodes := st.Assignments[d]
		for len(nodes) < st.Masters {
			target := pickSlotCandidate(st, d, "")
			if target == nil {
				break
			}
			i := len(nodes)
			nodes = append(nodes, target.NodeID)
			st.Assignments[d] = nodes
			st.setSince(d, i, now)
			st.holds[target.NodeID] = append(st.holds[target.NodeID], d)
			delete(st.TTLOverdue, d)
			s.switches++
			pos := st.positionOf(target)
			s.masterElected(target.NodeID, target.IP, d,
				fmt.Sprintf("queue #%d, additional master", pos)+st.slotDetail(i))
			s.changes = append(s.changes, domainChange{Domain: d, FromID: "", ToID: target.NodeID, ToIP: target.IP})
			log.Printf("domain %s: additional master %q %s", d, target.NodeID, st.slotLabel(i))
		}
		// masters_per_domain уменьшился: лишние слоты снимаем, чтобы набор
		// A-записей домена сжался, а не «залипал» до отказа держателя.
		if len(nodes) > st.Masters {
			for len(nodes) > st.Masters {
				i := len(nodes) - 1
				dropped := nodes[i]
				nodes = nodes[:i]
				if since := st.AssignmentsSince[d]; len(since) > i {
					st.AssignmentsSince[d] = since[:i]
				}
				if dropped != "" {
					ip := ""
					if h := st.all[dropped]; h != nil {
						ip = h.IP
					}
					s.masterLost(dropped, ip, d, "slot removed: masters_per_domain decreased")
					st.holds[dropped] = dropDomain(st.holds[dropped], d)
				}
			}
			st.Assignments[d] = nodes
			delete(st.TTLOverdue, d)
			log.Printf("domain %s: trimmed surplus master slots to %d (masters_per_domain decreased)", d, len(nodes))
			// Непустой ToID → DNS-цикл перепишет набор домена (сжатие A-записей).
			if len(nodes) > 0 {
				keeper := nodes[0]
				keeperIP := ""
				if h := st.all[keeper]; h != nil {
					keeperIP = h.IP
				}
				s.changes = append(s.changes, domainChange{Domain: d, FromID: "", ToID: keeper, ToIP: keeperIP})
			}
		}
	}

	// (b) ребаланс: домен целиком переходит к ноде без доменов
	for {
		var idle *Candidate
		for _, c := range st.queue {
			if len(st.holds[c.NodeID]) == 0 {
				idle = c
				break
			}
		}
		if idle == nil {
			return
		}
		var donor *Candidate
		for _, c := range st.queue {
			if len(st.holds[c.NodeID]) > 1 && (donor == nil || len(st.holds[c.NodeID]) > len(st.holds[donor.NodeID])) {
				donor = c
			}
		}
		if donor == nil {
			return
		}
		sort.Strings(st.holds[donor.NodeID])
		d := st.holds[donor.NodeID][0] // лексикографически первый у самого нагруженного
		// Мульти-мастер: переносим ОДИН слот домена на idle-ноду, а не весь
		// домен. Иначе домен на M слотов схлопывается в 1 (DNS-чехарда).
		// Слот выбираем так, чтобы не нарушить инвариант разных IP.
		slots := st.Assignments[d]
		idx := -1
		for i, id := range slots {
			ip := ""
			if h := st.all[id]; h != nil {
				ip = h.IP
			}
			if id != idle.NodeID && ip != idle.IP {
				idx = i
				break
			}
		}
		if idx == -1 {
			return // подвинуть нельзя без дубля IP/ноды — оставляем как есть
		}
		from := slots[idx]
		fromIP := ""
		if h := st.all[from]; h != nil {
			fromIP = h.IP
		}
		s.masterLost(from, fromIP, d, "rebalance: slot moved to a node with zero domains")
		st.holds[from] = dropDomain(st.holds[from], d)
		slots[idx] = idle.NodeID
		st.Assignments[d] = slots
		since := st.AssignmentsSince[d]
		for len(since) < len(slots) {
			since = append(since, time.Time{})
		}
		since[idx] = now
		st.AssignmentsSince[d] = since
		st.holds[idle.NodeID] = append(st.holds[idle.NodeID], d)
		delete(st.TTLOverdue, d)
		s.switches++
		s.masterElected(idle.NodeID, idle.IP, d, fmt.Sprintf("rebalance from %q", donor.NodeID))
		s.changes = append(s.changes, domainChange{Domain: d, FromID: from, ToID: idle.NodeID, ToIP: idle.IP})
		log.Printf("domain %s: rebalanced slot %q -> %q (idle node)", d, from, idle.NodeID)
	}
}

// reconcileStints — мастерство = держишь ≥1 домен, будучи здоровым.
// Держатель нездоров при пустой очереди: записи остаются на нём, но stint
// закрывается («мастерство потеряно»).
func reconcileStints(all map[string]*Candidate, healthy map[string]*Candidate, holds map[string][]string, now time.Time, s *selectionSink) {
	for _, c := range all {
		holdsDomains := len(holds[c.NodeID]) > 0
		healthyNow := healthy[c.NodeID] != nil
		switch {
		case holdsDomains && healthyNow && c.MasterSince.IsZero():
			c.MasterSince = now
			c.MasterStints++
		case (!holdsDomains || !healthyNow) && !c.MasterSince.IsZero():
			closeMasterStintLocked(c, now)
			if holdsDomains && !healthyNow && len(healthy) == 0 {
				s.events = append(s.events, state.Event{
					Type: state.EventMasterLost, NodeID: c.NodeID, IP: c.IP,
					Detail: "unhealthy, no healthy replacement — A-records left on it (" + strings.Join(holds[c.NodeID], ", ") + ")",
				})
			}
		}
	}
}
