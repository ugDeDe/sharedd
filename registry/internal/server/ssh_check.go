package server

// SSH-проверка мастеров ([ssh_check], internal/checkhost): раз в
// interval_sec регистратор через check-host.net проверяет TCP на порт
// (по умолчанию 22) у каждого мастера и у уже выведенных этой проверкой нод
// — со всех российских узлов check-host и foreign_nodes зарубежных.
//
// Проверка плохая, когда из РФ порт открылся меньше чем у ru_ok_percent
// узлов, а снаружи — у большинства: так выглядит блокировка IP из России.
// fail_streak плохих подряд — нода вне очереди (SSHBlocked); первая проверка,
// где из РФ порт открылся, возвращает её. Закрыто и снаружи (нода лежит или
// порт закрыт фаерволом) или узлы не ответили — проверка не в счёт: такие
// случаи решают TCP-проба, метрики и Globalping.
//
// Порт прокси для этого не годится: на нём антискан и лимиты новых
// соединений, а блокировку IP ТСПУ видно на любом порту.

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"sharedd/registry/internal/checkhost"
	"sharedd/registry/internal/config"
)

const (
	sshCheckNodesTTL   = time.Hour // список узлов check-host
	sshCheckPollEvery  = 3 * time.Second
	sshCheckPollWithin = 40 * time.Second // дольше — проверка не в счёт
)

// sshCheckAPI — шов для тестов.
type sshCheckAPI interface {
	Nodes(ctx context.Context) ([]checkhost.Node, error)
	StartTCP(ctx context.Context, host string, port int, nodes []string) (string, error)
	Result(ctx context.Context, id string) (map[string]checkhost.TCPResult, error)
}

var newSSHCheckAPI = func(base string) sshCheckAPI { return checkhost.New(base) }

// sshCheckFirstPollVar — первый опрос результата; var ради тестов.
var sshCheckFirstPollVar = 5 * time.Second

type sshCheckRuntime struct {
	mu       sync.Mutex
	inflight map[string]bool
	nodes    []checkhost.Node
	nodesAt  time.Time
	rr       int // ротация зарубежных узлов
}

type sshCheckSettings struct {
	enabled      bool
	interval     time.Duration
	port         int
	failStreak   int
	foreignNodes int
	ruOKPercent  int
	base         string
}

func (r *Registry) sshCheckSettings() sshCheckSettings {
	r.cfgMu.RLock()
	defer r.cfgMu.RUnlock()
	c := r.cfg.SSHCheck
	st := sshCheckSettings{
		enabled: c.Enabled == nil || *c.Enabled, interval: time.Duration(c.IntervalSec) * time.Second,
		port: c.Port, failStreak: c.FailStreak, foreignNodes: c.ForeignNodes, ruOKPercent: c.RUOKPercent, base: c.APIBase,
	}
	if st.interval < 10*time.Second {
		st.interval = config.DefaultSSHCheckIntervalSec * time.Second
	}
	if st.port <= 0 {
		st.port = config.DefaultSSHCheckPort
	}
	if st.failStreak <= 0 {
		st.failStreak = config.DefaultSSHCheckFailStreak
	}
	if st.foreignNodes <= 0 {
		st.foreignNodes = config.DefaultSSHCheckForeignNodes
	}
	if st.ruOKPercent <= 0 {
		st.ruOKPercent = config.DefaultSSHCheckRUOKPercent
	}
	return st
}

func (r *Registry) sshCheckLoop(ctx context.Context) {
	for {
		st := r.sshCheckSettings()
		if st.enabled {
			r.sshCheckTick(ctx, st)
		}
		t := time.NewTimer(st.interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

type sshCheckTarget struct {
	nodeID     string
	ip         string
	generation uint64
}

// sshCheckTick — запуск проверок мастеров и выведенных нод; проверка ноды,
// ещё идущая с прошлого тика, не дублируется.
func (r *Registry) sshCheckTick(ctx context.Context, st sshCheckSettings) {
	r.mu.RLock()
	var targets []sshCheckTarget
	for id, c := range r.state.Candidates {
		if c.IP != "" && (c.SSHBlocked || len(r.masterDomainsLocked(id)) > 0) {
			targets = append(targets, sshCheckTarget{nodeID: id, ip: c.IP, generation: c.Generation})
		}
	}
	r.mu.RUnlock()
	sort.Slice(targets, func(i, j int) bool { return targets[i].nodeID < targets[j].nodeID })
	for _, t := range targets {
		r.ssh.mu.Lock()
		if r.ssh.inflight == nil {
			r.ssh.inflight = map[string]bool{}
		}
		busy := r.ssh.inflight[t.nodeID]
		r.ssh.inflight[t.nodeID] = true
		r.ssh.mu.Unlock()
		if busy {
			continue
		}
		go func(t sshCheckTarget) {
			defer func() {
				r.ssh.mu.Lock()
				delete(r.ssh.inflight, t.nodeID)
				r.ssh.mu.Unlock()
			}()
			r.sshCheckNode(ctx, st, t)
		}(t)
	}
}

// sshCheckPickNodes — все российские узлы и foreign зарубежных по кругу.
func (r *Registry) sshCheckPickNodes(ctx context.Context, api sshCheckAPI, foreign int) (ru, abroad []string) {
	r.ssh.mu.Lock()
	nodes, fresh := r.ssh.nodes, time.Since(r.ssh.nodesAt) < sshCheckNodesTTL
	r.ssh.mu.Unlock()
	if !fresh {
		if got, err := api.Nodes(ctx); err == nil && len(got) > 0 {
			nodes = got
			r.ssh.mu.Lock()
			r.ssh.nodes, r.ssh.nodesAt = got, time.Now()
			r.ssh.mu.Unlock()
		} else if err != nil {
			log.Printf("ssh check: check-host nodes: %v (using cached list of %d)", err, len(nodes))
		}
	}
	var others []string
	for _, n := range nodes {
		if n.Country == "ru" {
			ru = append(ru, n.Name)
		} else {
			others = append(others, n.Name)
		}
	}
	if len(others) == 0 {
		return ru, nil
	}
	r.ssh.mu.Lock()
	start := r.ssh.rr
	r.ssh.rr = (r.ssh.rr + foreign) % len(others)
	r.ssh.mu.Unlock()
	for i := 0; i < foreign && i < len(others); i++ {
		abroad = append(abroad, others[(start+i)%len(others)])
	}
	return ru, abroad
}

type sshCheckOutcome struct {
	ruOK, ruDone   int
	abOK, abDone   int
	ruErrors       []string
	bad, good      bool
	summary        string
	inconclusive   bool
	inconclusiveBy string
}

// sshCheckEvaluate — вердикт по результатам узлов.
func sshCheckEvaluate(res map[string]checkhost.TCPResult, ru, abroad []string, ruOKPercent int) sshCheckOutcome {
	var o sshCheckOutcome
	errs := map[string]bool{}
	for _, n := range ru {
		r, ok := res[n]
		if !ok || r.Pending {
			continue
		}
		o.ruDone++
		if r.OK {
			o.ruOK++
		} else if r.Error != "" {
			errs[r.Error] = true
		}
	}
	for _, n := range abroad {
		r, ok := res[n]
		if !ok || r.Pending {
			continue
		}
		o.abDone++
		if r.OK {
			o.abOK++
		}
	}
	for e := range errs {
		o.ruErrors = append(o.ruErrors, e)
	}
	sort.Strings(o.ruErrors)
	o.summary = fmt.Sprintf("RU %d/%d, abroad %d/%d", o.ruOK, o.ruDone, o.abOK, o.abDone)
	if len(o.ruErrors) > 0 {
		o.summary += " (" + strings.Join(o.ruErrors, ", ") + ")"
	}
	switch {
	case o.ruDone == 0 || o.abDone == 0:
		o.inconclusive, o.inconclusiveBy = true, "no results"
	case o.ruOK*100 >= ruOKPercent*o.ruDone:
		o.good = true
	case o.abOK*2 > o.abDone:
		o.bad = true
	default:
		o.inconclusive, o.inconclusiveBy = true, "closed from abroad too"
	}
	return o
}

func (r *Registry) sshCheckNode(ctx context.Context, st sshCheckSettings, t sshCheckTarget) {
	api := newSSHCheckAPI(st.base)
	ru, abroad := r.sshCheckPickNodes(ctx, api, st.foreignNodes)
	if len(ru) == 0 || len(abroad) == 0 {
		return
	}
	id, err := api.StartTCP(ctx, t.ip, st.port, append(append([]string(nil), ru...), abroad...))
	if err != nil {
		log.Printf("ssh check of %s (%s): %v", t.nodeID, t.ip, err)
		return
	}
	var res map[string]checkhost.TCPResult
	deadline := time.Now().Add(sshCheckPollWithin)
	wait := sshCheckFirstPollVar
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = sshCheckPollEvery
		got, err := api.Result(ctx, id)
		if err == nil {
			res = got
			pending := false
			for _, n := range append(append([]string(nil), ru...), abroad...) {
				if v, ok := got[n]; !ok || v.Pending {
					pending = true
					break
				}
			}
			if !pending {
				break
			}
		}
		if time.Now().After(deadline) {
			break
		}
	}
	r.applySSHCheck(t, sshCheckEvaluate(res, ru, abroad, st.ruOKPercent), st.failStreak, time.Now())
}

// applySSHCheck — серия и вывод/возврат ноды. Нода сменилась (IP,
// перерегистрация) — результат устарел и не применяется.
func (r *Registry) applySSHCheck(t sshCheckTarget, o sshCheckOutcome, failStreak int, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.state.Candidates[t.nodeID]
	if c == nil || c.IP != t.ip || c.Generation != t.generation {
		return
	}
	c.SSHAt, c.SSHSummary = now, o.summary
	changed := false
	switch {
	case o.bad:
		c.SSHFailStreak++
		changed = true
		if c.SSHFailStreak >= failStreak && !c.SSHBlocked {
			c.SSHBlocked = true
			r.addEventLocked(Event{
				Type: EventSSHBlocked, NodeID: c.NodeID, IP: c.IP,
				Detail: fmt.Sprintf("порт закрыт из РФ %d проверок подряд, снаружи открыт: %s — вне очереди", c.SSHFailStreak, o.summary),
			})
			log.Printf("ssh check: %s (%s) out of the queue — %d bad checks in a row: %s", c.NodeID, c.IP, c.SSHFailStreak, o.summary)
		}
	case o.good:
		if c.SSHFailStreak != 0 || c.SSHBlocked {
			changed = true
		}
		c.SSHFailStreak = 0
		if c.SSHBlocked {
			c.SSHBlocked = false
			r.addEventLocked(Event{
				Type: EventSSHRecovered, NodeID: c.NodeID, IP: c.IP,
				Detail: "порт снова открыт из РФ: " + o.summary,
			})
			log.Printf("ssh check: %s (%s) reachable from RU again: %s", c.NodeID, c.IP, o.summary)
		}
	}
	if changed {
		r.persistStateLocked()
	}
}
