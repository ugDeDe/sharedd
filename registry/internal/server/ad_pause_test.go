package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sharedd/registry/internal/machineapi"
)

const testPoolTag = "00112233445566778899aabbccddeeff"

// Доля сходится к заданной и реклама чередуется: при share < 50% два
// рекламных периода подряд не бывает.
func TestAdPauseDecideConvergesAndAlternates(t *testing.T) {
	for _, share := range []float64{0.05, 0.1, 0.25, 0.5} {
		period := 10 * time.Minute
		var ad, master int64
		prevAd := false
		for i := 0; i < 1000; i++ {
			on := adPauseDecide(ad, master, period, share)
			if on && prevAd && share < 0.5 {
				t.Fatalf("share %.2f: two ad periods in a row at %d", share, i)
			}
			if on {
				ad += int64(period.Seconds())
			}
			master += int64(period.Seconds())
			prevAd = on
		}
		got := float64(ad) / float64(master)
		if got < share-0.01 || got > share+0.01 {
			t.Fatalf("share %.2f: converged to %.4f", share, got)
		}
	}
}

func adPauseRegistry(t *testing.T) *Registry {
	t.Helper()
	r := newTestRegistry(t)
	on := true
	r.cfg.AdPause.Enabled = &on
	r.cfg.AdPause.Tag = testPoolTag
	r.cfg.AdPause.SharePercent = 10
	ttl := 10
	r.cfg.Rotation.MasterTTLMinutes = &ttl
	return r
}

func TestAdPauseScheduleOnlyInsideMasteryAndSupported(t *testing.T) {
	r := adPauseRegistry(t)
	now := time.Unix(1_800_000_000, 0)
	// Мастер давно и без рекламы — недобор, период рекламный.
	r.state.Candidates["m"] = &Candidate{NodeID: "m", MasterSince: now.Add(-time.Minute), MasterSeconds: 3600}
	r.state.Candidates["idle"] = &Candidate{NodeID: "idle", MasterSeconds: 3600}
	r.state.Candidates["old"] = &Candidate{NodeID: "old", MasterSince: now.Add(-time.Minute), MasterSeconds: 3600,
		AdState: machineapi.AdPauseStateUnsupported}
	st := r.adPauseSettings()
	if !r.scheduleAdPauseLocked(now, st) {
		t.Fatal("first decision must change state")
	}
	if !r.state.Candidates["m"].AdActive || r.state.Candidates["idle"].AdActive || r.state.Candidates["old"].AdActive {
		t.Fatalf("only the supported master may get an ad period: m=%v idle=%v old=%v",
			r.state.Candidates["m"].AdActive, r.state.Candidates["idle"].AdActive, r.state.Candidates["old"].AdActive)
	}
	// Внутри периода решение не пересматривается.
	if r.scheduleAdPauseLocked(now.Add(5*time.Minute), st) {
		t.Fatal("decision must hold for the whole period")
	}
	// Конец мастерства снимает рекламу сразу.
	r.state.Candidates["m"].MasterSince = time.Time{}
	r.scheduleAdPauseLocked(now.Add(6*time.Minute), st)
	if r.state.Candidates["m"].AdActive {
		t.Fatal("ad must stop when mastery ends")
	}
	// Выключение паузы снимает рекламу у всех.
	r.state.Candidates["m"].MasterSince = now.Add(7 * time.Minute)
	off := false
	r.cfg.AdPause.Enabled = &off
	r.scheduleAdPauseLocked(now.Add(8*time.Minute), r.adPauseSettings())
	if r.state.Candidates["m"].AdActive {
		t.Fatal("disabled ad pause must not keep the pool tag")
	}
}

func heartbeat(t *testing.T, r *Registry, body string) machineapi.HeartbeatResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	r.buildMux().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/heartbeat", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat %s: %d %s", body, rec.Code, rec.Body.String())
	}
	var resp machineapi.HeartbeatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("heartbeat body: %v", err)
	}
	return resp
}

func TestAdPauseHeartbeatDirectiveAndConfirmedAccounting(t *testing.T) {
	r := adPauseRegistry(t)
	r.state.Candidates["node-a1b2c"] = &Candidate{NodeID: "node-a1b2c", IP: "1.2.3.4", AdActive: true}

	resp := heartbeat(t, r, `{"node_id":"node-a1b2c"}`) // старый агент: поле не шлёт
	if resp.AdPause == nil || !resp.AdPause.Active || resp.AdPause.Tag != testPoolTag {
		t.Fatalf("directive must carry the pool tag: %+v", resp.AdPause)
	}
	if c := r.state.Candidates["node-a1b2c"]; !c.AdSince.IsZero() || c.AdState != "" {
		t.Fatal("without confirmation nothing may be counted")
	}

	heartbeat(t, r, `{"node_id":"node-a1b2c","ad_pause_state":"pool"}`)
	c := r.state.Candidates["node-a1b2c"]
	if c.AdSince.IsZero() || c.AdState != machineapi.AdPauseStatePool {
		t.Fatalf("confirmed pool tag must open a span: %+v", c)
	}
	c.AdSince = c.AdSince.Add(-90 * time.Second) // время прошло
	c.AdActive = false
	resp = heartbeat(t, r, `{"node_id":"node-a1b2c","ad_pause_state":"own"}`)
	if resp.AdPause == nil || resp.AdPause.Active {
		t.Fatalf("directive must return the own tag: %+v", resp.AdPause)
	}
	if c.AdSeconds < 90 || !c.AdSince.IsZero() {
		t.Fatalf("closed span must be counted: sec=%d since=%v", c.AdSeconds, c.AdSince)
	}
	var on, off int
	for _, ev := range r.state.Events {
		switch ev.Type {
		case "ad_pause_on":
			on++
		case "ad_pause_off":
			off++
		}
	}
	if on != 1 || off != 1 {
		t.Fatalf("each switch must be journaled once: on=%d off=%d", on, off)
	}
}

func TestAdPausePanelValidation(t *testing.T) {
	r := newTestRegistryWithConfigFile(t)
	rec, _ := putConfig(t, r, `{"ad_pause":{"enabled":true,"tag":" 00112233445566778899AABBCCDDEEFF ","share_percent":15}}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT ad_pause: %d %s", rec.Code, rec.Body.String())
	}
	if r.cfg.AdPause.Tag != testPoolTag || r.cfg.AdPause.SharePercent != 15 || !*r.cfg.AdPause.Enabled {
		t.Fatalf("ad_pause not applied: %+v", r.cfg.AdPause)
	}
	for _, body := range []string{
		`{"ad_pause":{"enabled":true,"tag":"","share_percent":10}}`,
		`{"ad_pause":{"enabled":true,"tag":"00000000000000000000000000000000","share_percent":10}}`,
		`{"ad_pause":{"enabled":false,"tag":"","share_percent":0}}`,
		`{"ad_pause":{"enabled":false,"tag":"","share_percent":51}}`,
	} {
		if rec, _ := putConfig(t, r, body, ""); rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid ad_pause %s accepted: %d", body, rec.Code)
		}
	}
}
