package server

// Чистое ядро селекции мастеров: функции принимают явные входы (очередь,
// назначения, счётчики) и не знают про Registry, локи, DNS и персистенс.
// Оркестратор — evaluateAssignments (ниже): собирает view из state, гоняет
// проходы, сбрасывает sink в журнал/счётчики/DNS.
//
// Инварианты (см. также комментарий к evaluateAssignments):
// - очередь по непрерывному здоровью, позиция = QueuedAt;
// - живой держатель домена не трогается вне принудительной TTL-ротации;
// - пустой очереди назначений не снимает (записи остаются на последних
//   мастера́х);
// - детерминизм: сортировка очереди и доменов полная (тай-брейки по
//   RegisteredAt/NodeID), обход доменов лексикографический.

import (
	"fmt"
	"log"
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
// мутируют их напрямую, как раньше делал монолит).
type selectionView struct {
	queue   []*Candidate          // здоровые, отсортированы по старшинству в очереди
	healthy map[string]*Candidate // nodeID → кандидат (только queue)
	all     map[string]*Candidate // nodeID → кандидат (весь пул, для IP держателей)
	holds   map[string][]string   // nodeID → домены держателя

	Assignments      map[string]string
	AssignmentsSince map[string]time.Time
	TTLOverdue       map[string]bool
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

// pickLeastLoaded — наименее загруженный из очереди; при равенстве — раньше
// в очереди. exclude ("") — держатель, которого нельзя выбирать (TTL).
func pickLeastLoaded(st *selectionView, exclude string) *Candidate {
	var best *Candidate
	for _, c := range st.queue {
		if c.NodeID == exclude {
			continue
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

// rotateByTTL — pass 0: принудительная ротация по TTL мастерства. Здоровый
// держатель с истёкшим лимитом сдаёт домен наименее загруженной ноде (кроме
// себя); замены нет — домен остаётся, OVERDUE логируется раз за эпизод.
func rotateByTTL(st *selectionView, domains []string, ttl time.Duration, now time.Time, s *selectionSink) {
	if ttl <= 0 || len(st.queue) == 0 {
		return
	}
	for _, d := range domains {
		holderID := st.Assignments[d]
		if holderID == "" || st.healthy[holderID] == nil {
			delete(st.TTLOverdue, d)
			continue // мёртвых раздаст pass 1
		}
		since := st.AssignmentsSince[d]
		if since.IsZero() {
			// назначение досталось от старой версии — отсчёт отныне
			st.AssignmentsSince[d] = now
			continue
		}
		age := now.Sub(since)
		if age < ttl {
			continue
		}
		target := pickLeastLoaded(st, holderID)
		if target == nil {
			if !st.TTLOverdue[d] {
				st.TTLOverdue[d] = true
				log.Printf("domain %s: master TTL %s exceeded (%s old) but no healthy replacement — keeping %q",
					d, ttl, age.Round(time.Second), holderID)
			}
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
		st.Assignments[d] = target.NodeID
		st.AssignmentsSince[d] = now
		delete(st.TTLOverdue, d)
		s.switches++
		s.ttlRotations++
		st.toTail(holderID, now)
		pos := st.positionOf(target)
		s.masterElected(target.NodeID, target.IP, d,
			fmt.Sprintf("forced rotation (TTL %s), queue #%d, was %q", ttl, pos, holderID))
		s.changes = append(s.changes, domainChange{Domain: d, FromID: holderID, ToID: target.NodeID, ToIP: target.IP})
		log.Printf("domain %s: master %q -> %q (%s)", d, holderID, target.NodeID, reason)
	}
}

// reassignDead — pass 1: переназначение мёртвых/отсутствующих мастеров.
// Живой держатель — домен не трогаем (стабильность >> симметрия).
func reassignDead(st *selectionView, domains []string, freshness time.Duration, now time.Time, s *selectionSink) {
	if len(st.queue) == 0 {
		return
	}
	for _, d := range domains {
		holderID := st.Assignments[d]
		if holderID != "" && st.healthy[holderID] != nil {
			continue // держатель жив
		}
		var reason, holderIP string
		switch holder := st.all[holderID]; {
		case holderID == "":
			reason = "domain had no master"
		case holder == nil:
			reason = "assignee removed from pool"
		default:
			reason = holder.UnhealthyReason(freshness)
			holderIP = holder.IP
		}
		if holderID != "" {
			s.masterLost(holderID, holderIP, d, reason)
			st.holds[holderID] = dropDomain(st.holds[holderID], d)
		}
		target := pickLeastLoaded(st, "")
		st.holds[target.NodeID] = append(st.holds[target.NodeID], d)
		st.Assignments[d] = target.NodeID
		st.AssignmentsSince[d] = now
		delete(st.TTLOverdue, d)
		s.switches++
		pos := st.positionOf(target)
		s.masterElected(target.NodeID, target.IP, d,
			fmt.Sprintf("queue #%d (was %q: %s)", pos, holderID, reason))
		s.changes = append(s.changes, domainChange{Domain: d, FromID: holderID, ToID: target.NodeID, ToIP: target.IP})
		log.Printf("domain %s: master %q -> %q (%s)", d, holderID, target.NodeID, reason)
	}
}

// fillEmpty — pass 2: сироты перетекают к нодам без доменов. Балансировки
// 3/1 → 2/2 НЕТ сознательно: лишний DNS-черн не оправдан.
func fillEmpty(st *selectionView, now time.Time, s *selectionSink) {
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
		st.holds[donor.NodeID] = st.holds[donor.NodeID][1:]
		st.holds[idle.NodeID] = append(st.holds[idle.NodeID], d)
		st.Assignments[d] = idle.NodeID
		st.AssignmentsSince[d] = now
		delete(st.TTLOverdue, d)
		s.switches++
		s.masterLost(donor.NodeID, donor.IP, d, "rebalance: domain moved to a node with zero domains")
		s.masterElected(idle.NodeID, idle.IP, d, fmt.Sprintf("rebalance from %q", donor.NodeID))
		s.changes = append(s.changes, domainChange{Domain: d, FromID: donor.NodeID, ToID: idle.NodeID, ToIP: idle.IP})
		log.Printf("domain %s: rebalanced %q -> %q (idle node)", d, donor.NodeID, idle.NodeID)
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
