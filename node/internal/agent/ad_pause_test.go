package agent

import (
	"strings"
	"testing"
	"time"

	"sharedd/node/internal/config"
)

const testPoolAdTag = "00112233445566778899aabbccddeeff"

func TestApplyAdPauseOnMtdd(t *testing.T) {
	f := newMtddFixture(t, `{"users":[]}`)
	if got := applyAdPause(f.nodeCfg, adPauseDirective{Active: true, Tag: strings.ToUpper(testPoolAdTag)}); got != adPausePool {
		t.Fatalf("set pool tag: %s", got)
	}
	if got := applyAdPause(f.nodeCfg, adPauseDirective{}); got != adPauseOwn {
		t.Fatalf("restore own tag: %s", got)
	}
	log := f.callLog()
	if !strings.Contains(log, `POST /v1/ad-tag {"ad_tag":"`+testPoolAdTag+`"}`) || !strings.Contains(log, "DELETE /v1/ad-tag") {
		t.Fatalf("unexpected mtdd calls:\n%s", log)
	}
	if got := applyAdPause(f.nodeCfg, adPauseDirective{Active: true, Tag: "zz"}); got != adPauseFailed {
		t.Fatalf("invalid tag must fail without touching mtdd: %s", got)
	}

	f.mu.Lock()
	f.noAdTag = true
	f.mu.Unlock()
	if got := applyAdPause(f.nodeCfg, adPauseDirective{Active: true, Tag: testPoolAdTag}); got != adPauseUnsupported {
		t.Fatalf("mtdd without /v1/ad-tag must be unsupported: %s", got)
	}
}

func TestApplyAdPauseNonMtddUnsupported(t *testing.T) {
	f := newMtddFixture(t, `{"users":[]}`)
	f.nodeCfg.Telemt.ConfigPath = "/etc/telemt/telemt.toml"
	oldOn := adPauseOnMtproxyl
	t.Cleanup(func() { adPauseOnMtproxyl = oldOn })
	adPauseOnMtproxyl = func() bool { return false }
	if got := applyAdPause(f.nodeCfg, adPauseDirective{Active: true, Tag: testPoolAdTag}); got != adPauseUnsupported {
		t.Fatalf("telemt node: %s", got)
	}
	if f.callLog() != "" {
		t.Fatal("non-mtdd node must not call the mtdd API")
	}
}

func TestAdPauseNeedsApply(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	on := adPauseDirective{Active: true, Tag: testPoolAdTag}
	off := adPauseDirective{}
	cases := []struct {
		name      string
		d         adPauseDirective
		state     string
		attempted adPauseDirective
		failed    bool
		at        time.Time
		want      bool
	}{
		{"first run clears a stale override", off, "", off, false, now, true},
		{"unchanged directive is idle", on, adPausePool, on, false, now.Add(-time.Hour), false},
		{"new directive applies", off, adPausePool, on, false, now, true},
		{"failure retries after a pause", on, adPauseFailed, on, true, now.Add(-adPauseRetry), true},
		{"failure waits before retry", on, adPauseFailed, on, true, now.Add(-time.Second), false},
		{"unsupported rechecked rarely", off, adPauseUnsupported, off, false, now.Add(-adPauseRecheckUnsp), true},
		{"unsupported not hammered", off, adPauseUnsupported, off, false, now.Add(-time.Minute), false},
	}
	for _, tc := range cases {
		if got := adPauseNeedsApply(tc.d, tc.state, tc.attempted, tc.failed, tc.at, now); got != tc.want {
			t.Fatalf("%s: got %v", tc.name, got)
		}
	}
}

func resetAdPause(t *testing.T) {
	t.Helper()
	oldApply := applyAdPause
	t.Cleanup(func() {
		applyAdPause = oldApply
		adPause.mu.Lock()
		adPause.supported, adPause.state, adPause.attempted, adPause.failed, adPause.at = false, "", adPauseDirective{}, false, time.Time{}
		adPause.mu.Unlock()
	})
	adPause.mu.Lock()
	adPause.supported, adPause.state, adPause.attempted, adPause.failed, adPause.at = false, "", adPauseDirective{}, false, time.Time{}
	adPause.mu.Unlock()
}

// Неудачный возврат своего тега не должен прятать время с тегом пула:
// агент продолжает подтверждать pool, пока тег реально не вернётся.
func TestAdPauseFailedRestoreKeepsConfirmingPool(t *testing.T) {
	resetAdPause(t)
	results := []string{adPausePool, adPauseFailed, adPauseOwn}
	applyAdPause = func(_ *config.NodeConfig, _ adPauseDirective) string {
		r := results[0]
		results = results[1:]
		return r
	}
	now := time.Unix(1_800_000_000, 0)
	adPause.supported = true
	adPauseStep(nil, adPauseDirective{Active: true, Tag: testPoolAdTag}, now)
	if got := adPauseHeartbeatState(); got != adPausePool {
		t.Fatalf("after set: %s", got)
	}
	adPauseStep(nil, adPauseDirective{}, now.Add(time.Minute))
	if got := adPauseHeartbeatState(); got != adPausePool {
		t.Fatalf("failed restore must keep confirming pool, got %s", got)
	}
	adPauseStep(nil, adPauseDirective{}, now.Add(time.Minute+adPauseRetry))
	if got := adPauseHeartbeatState(); got != adPauseOwn {
		t.Fatalf("retried restore: %s", got)
	}
}

// Прежнему регистратору (пустое тело) поле не шлётся, а тег возвращается свой.
func TestHeartbeatResponseGatesStateAndQueuesDirective(t *testing.T) {
	resetAdPause(t)
	adPause.state = adPausePool
	noteHeartbeatResponse([]byte(""))
	if adPauseHeartbeatState() != "" {
		t.Fatal("old registry must not receive ad_pause_state")
	}
	if d := <-adPause.desired; d.Active {
		t.Fatal("old registry implies the own tag")
	}
	noteHeartbeatResponse([]byte(`{"ad_pause":{"active":true,"tag":"` + testPoolAdTag + `"}}`))
	if adPauseHeartbeatState() != adPausePool {
		t.Fatal("registry with ad_pause must receive the state")
	}
	if d := <-adPause.desired; !d.Active || d.Tag != testPoolAdTag {
		t.Fatalf("directive: %+v", d)
	}
}
