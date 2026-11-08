package server

import (
	"context"
	"testing"
	"time"

	"sharedd/registry/internal/checkhost"
)

func chRes(ok bool, err string) checkhost.TCPResult { return checkhost.TCPResult{OK: ok, Error: err} }

func TestSSHCheckEvaluate(t *testing.T) {
	ru := []string{"ru1", "ru2"}
	ab := []string{"de1", "nl1", "fi1"}
	timeout := chRes(false, "Connection timed out")
	for _, tc := range []struct {
		name           string
		res            map[string]checkhost.TCPResult
		bad, good, inc bool
	}{
		{"blocked from RU", map[string]checkhost.TCPResult{"ru1": timeout, "ru2": timeout, "de1": chRes(true, ""), "nl1": chRes(true, ""), "fi1": timeout}, true, false, false},
		{"open from RU", map[string]checkhost.TCPResult{"ru1": chRes(true, ""), "ru2": timeout, "de1": chRes(true, ""), "nl1": chRes(true, ""), "fi1": chRes(true, "")}, false, true, false},
		{"closed everywhere", map[string]checkhost.TCPResult{"ru1": timeout, "ru2": timeout, "de1": timeout, "nl1": timeout, "fi1": chRes(true, "")}, false, false, true},
		{"no results", map[string]checkhost.TCPResult{"ru1": {Pending: true}}, false, false, true},
	} {
		o := sshCheckEvaluate(tc.res, ru, ab, 50)
		if o.bad != tc.bad || o.good != tc.good || o.inconclusive != tc.inc {
			t.Fatalf("%s: %+v", tc.name, o)
		}
	}
}

// Три плохих подряд — вне очереди; первая хорошая — назад.
func TestSSHCheckBlocksAfterStreakAndRecovers(t *testing.T) {
	r := newTestRegistry(t)
	r.state.Candidates["n"] = &Candidate{NodeID: "n", IP: "1.2.3.4", Generation: 7}
	tgt := sshCheckTarget{nodeID: "n", ip: "1.2.3.4", generation: 7}
	bad := sshCheckOutcome{bad: true, summary: "RU 0/2, abroad 3/3 (Connection timed out)"}
	neutral := sshCheckOutcome{inconclusive: true}
	now := time.Unix(1_800_000_000, 0)
	r.applySSHCheck(tgt, bad, 3, now)
	r.applySSHCheck(tgt, neutral, 3, now) // не в счёт и серию не рвёт
	r.applySSHCheck(tgt, bad, 3, now)
	if c := r.state.Candidates["n"]; c.SSHBlocked || c.SSHFailStreak != 2 {
		t.Fatalf("two bad checks: %+v", c)
	}
	r.applySSHCheck(tgt, bad, 3, now)
	c := r.state.Candidates["n"]
	if !c.SSHBlocked || c.IsFullyHealthy(time.Hour) {
		t.Fatal("third bad check must take the node out of the queue")
	}
	r.applySSHCheck(sshCheckTarget{nodeID: "n", ip: "5.6.7.8", generation: 7}, sshCheckOutcome{good: true}, 3, now)
	if !c.SSHBlocked {
		t.Fatal("result for another IP must not apply")
	}
	r.applySSHCheck(tgt, sshCheckOutcome{good: true, summary: "RU 2/2"}, 3, now)
	if c.SSHBlocked || c.SSHFailStreak != 0 {
		t.Fatalf("open from RU must return the node: %+v", c)
	}
	var blocked, recovered int
	for _, ev := range r.state.Events {
		switch ev.Type {
		case EventSSHBlocked:
			blocked++
		case EventSSHRecovered:
			recovered++
		}
	}
	if blocked != 1 || recovered != 1 {
		t.Fatalf("events: blocked=%d recovered=%d", blocked, recovered)
	}
}

// fakeCheckHost — check-host с фиксированными ответами.
type fakeCheckHost struct {
	nodes   []checkhost.Node
	started []string
	res     map[string]checkhost.TCPResult
}

func (f *fakeCheckHost) Nodes(context.Context) ([]checkhost.Node, error) { return f.nodes, nil }
func (f *fakeCheckHost) StartTCP(_ context.Context, host string, port int, nodes []string) (string, error) {
	f.started = append(f.started, host)
	return "id", nil
}
func (f *fakeCheckHost) Result(context.Context, string) (map[string]checkhost.TCPResult, error) {
	return f.res, nil
}

// Тик проверяет только мастеров и уже выведенные ноды.
func TestSSHCheckTargetsMastersAndBlocked(t *testing.T) {
	r := newTestRegistry(t)
	r.state.Candidates["m"] = &Candidate{NodeID: "m", IP: "1.1.1.1"}
	r.state.Candidates["q"] = &Candidate{NodeID: "q", IP: "2.2.2.2"}
	r.state.Candidates["b"] = &Candidate{NodeID: "b", IP: "3.3.3.3", SSHBlocked: true}
	r.state.Assignments = map[string]AssignmentSlots{"d1.example.com": {"m"}}
	f := &fakeCheckHost{
		nodes: []checkhost.Node{{Name: "ru1", Country: "ru"}, {Name: "de1", Country: "de"}},
		res:   map[string]checkhost.TCPResult{"ru1": chRes(true, ""), "de1": chRes(true, "")},
	}
	old := newSSHCheckAPI
	t.Cleanup(func() { newSSHCheckAPI = old })
	newSSHCheckAPI = func(string) sshCheckAPI { return f }
	oldFirst := sshCheckFirstPollVar
	sshCheckFirstPollVar = time.Millisecond
	t.Cleanup(func() { sshCheckFirstPollVar = oldFirst })
	st := sshCheckSettings{enabled: true, interval: time.Minute, port: 22, failStreak: 3, foreignNodes: 1, ruOKPercent: 50}
	for _, tg := range []sshCheckTarget{{"m", "1.1.1.1", 0}, {"b", "3.3.3.3", 0}} {
		r.sshCheckNode(context.Background(), st, tg)
	}
	r.sshCheckTick(context.Background(), st)
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.ssh.mu.Lock()
		n := len(r.ssh.inflight)
		r.ssh.mu.Unlock()
		if n == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, h := range f.started {
		if h == "2.2.2.2" {
			t.Fatal("a queued non-master must not be checked")
		}
	}
	if r.state.Candidates["b"].SSHBlocked {
		t.Fatal("open from RU must return the blocked node")
	}
}

func TestSSHCheckPanelEditor(t *testing.T) {
	r := newTestRegistryWithConfigFile(t)
	rec, _ := putConfig(t, r, `{"ssh_check":{"enabled":true,"interval_sec":30,"port":2222,"fail_streak":4,"foreign_nodes":2,"ru_ok_percent":60,"api_base":"https://check-host.net/"}}`, "")
	if rec.Code != 200 {
		t.Fatalf("PUT ssh_check: %d %s", rec.Code, rec.Body.String())
	}
	st := r.sshCheckSettings()
	if !st.enabled || st.interval != 30*time.Second || st.port != 2222 || st.failStreak != 4 || st.foreignNodes != 2 || st.ruOKPercent != 60 || st.base != "https://check-host.net" {
		t.Fatalf("ssh_check not applied: %+v", st)
	}
	for _, body := range []string{
		`{"ssh_check":{"enabled":true,"interval_sec":5,"port":22,"fail_streak":3,"foreign_nodes":3,"ru_ok_percent":50}}`,
		`{"ssh_check":{"enabled":true,"interval_sec":20,"port":0,"fail_streak":3,"foreign_nodes":3,"ru_ok_percent":50}}`,
		`{"ssh_check":{"enabled":true,"interval_sec":20,"port":22,"fail_streak":3,"foreign_nodes":11,"ru_ok_percent":50}}`,
	} {
		if rec, _ := putConfig(t, r, body, ""); rec.Code != 400 {
			t.Fatalf("invalid %s accepted: %d", body, rec.Code)
		}
	}
}

// banConfirmRegistry — карантин на одну попытку, check-host подменён.
func banConfirmRegistry(t *testing.T, res map[string]checkhost.TCPResult) *Registry {
	t.Helper()
	mock := newGPMock(t)
	mock.setBad(true)
	r := newTerminateTestRegistry(t)
	r.cfg.Globalping.APIBase = mock.URL
	r.cfg.QuarantineAttempts = 1
	on := true
	r.cfg.SSHCheck.Enabled = &on
	r.cfg.SSHCheck.IntervalSec, r.cfg.SSHCheck.Port, r.cfg.SSHCheck.FailStreak = 20, 22, 3
	r.cfg.SSHCheck.ForeignNodes, r.cfg.SSHCheck.RUOKPercent = 1, 50
	f := &fakeCheckHost{nodes: []checkhost.Node{{Name: "ru1", Country: "ru"}, {Name: "de1", Country: "de"}}, res: res}
	old, oldFirst := newSSHCheckAPI, sshCheckFirstPollVar
	t.Cleanup(func() { newSSHCheckAPI, sshCheckFirstPollVar = old, oldFirst })
	newSSHCheckAPI = func(string) sshCheckAPI { return f }
	sshCheckFirstPollVar = time.Millisecond
	r.register(registerRequest{NodeID: "node-one", IP: "1.1.1.1"})
	return r
}

// check-host подтвердил (из РФ закрыт, снаружи открыт) — бан.
func TestBanNeedsCheckHostConfirmation(t *testing.T) {
	r := banConfirmRegistry(t, map[string]checkhost.TCPResult{"ru1": chRes(false, "Connection timed out"), "de1": chRes(true, "")})
	sendReport(t, r, "node-one", true, true)
	if r.state.Candidates["node-one"] != nil || r.state.Terminated["node-one"] == nil {
		t.Fatal("confirmed block must ban")
	}
}

// Из РФ открыт — бана нет, нода остаётся в карантине.
func TestBanNotConfirmedWhenOpenFromRU(t *testing.T) {
	r := banConfirmRegistry(t, map[string]checkhost.TCPResult{"ru1": chRes(true, ""), "de1": chRes(true, "")})
	sendReport(t, r, "node-one", true, true)
	c := r.state.Candidates["node-one"]
	if c == nil || r.state.Terminated["node-one"] != nil || c.Quarantine == nil {
		t.Fatalf("unconfirmed block must keep the node in quarantine without a ban: %+v", c)
	}
	found := false
	for _, ev := range r.state.Events {
		found = found || ev.Type == EventBanUnconfirmed
	}
	if !found {
		t.Fatal("ban_unconfirmed must be journaled")
	}
}

// Закрыт и снаружи (или check-host молчит) — тоже без бана.
func TestBanNotConfirmedWhenClosedEverywhere(t *testing.T) {
	r := banConfirmRegistry(t, map[string]checkhost.TCPResult{"ru1": chRes(false, "Connection timed out"), "de1": chRes(false, "Connection timed out")})
	sendReport(t, r, "node-one", true, true)
	if r.state.Candidates["node-one"] == nil || r.state.Terminated["node-one"] != nil {
		t.Fatal("closed everywhere is not a confirmed block")
	}
}
