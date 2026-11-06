package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sharedd/registry/internal/machineapi"
)

const (
	testScanSrcA = "https://lists.example/a.txt"
	testScanSrcB = "https://lists.example/b.txt"
	testScanGeo  = "https://geo.example/ru.json"
)

// antiscanFixture — регистратор с фиктивными источниками; responses правится
// тестом между сборками, nil-значение = ошибка загрузки.
func antiscanFixture(t *testing.T) (*Registry, map[string][]byte) {
	t.Helper()
	r := newTestRegistry(t)
	r.cfg.Antiscan.Sources = []string{testScanSrcA, testScanSrcB}
	r.cfg.Antiscan.IntervalMin = 30
	r.cfg.Antiscan.GeoURL = testScanGeo
	responses := map[string][]byte{
		testScanGeo:  []byte(`{"status":"ok","data":{"resources":{"ipv4":["5.0.0.0/16","95.0.0.0-95.0.0.255"]}}}`),
		testScanSrcA: []byte("# ipsum\n5.0.1.1\t4\n8.8.8.8\t9\n95.0.0.0/23\n"),
		testScanSrcB: []byte("5.0.2.0/24\n1.1.1.1\n"),
	}
	r.scan.fetch = func(_ context.Context, url string) ([]byte, error) {
		if data := responses[url]; data != nil {
			return data, nil
		}
		return nil, errors.New("unreachable")
	}
	return r, responses
}

func antiscanGet(t *testing.T, r *Registry) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r.buildMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/antiscan", nil))
	return rec
}

func TestAntiscanKeepsOnlyRussianEntries(t *testing.T) {
	r, _ := antiscanFixture(t)
	r.refreshAntiscan(context.Background(), time.Unix(1_800_000_000, 0))
	rec := antiscanGet(t, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /antiscan: %d", rec.Code)
	}
	// 8.8.8.8 и 1.1.1.1 вне RU; 95.0.0.0/23 обрезан до RU-части /24.
	if got := rec.Body.String(); got != "5.0.1.1\n5.0.2.0/24\n95.0.0.0/24\n" {
		t.Fatalf("unexpected list:\n%s", got)
	}
	rev := rec.Header().Get(machineapi.AntiscanRevisionHeader)
	if rev == "" || rev != r.antiscanRevision() {
		t.Fatalf("revision header %q vs %q", rev, r.antiscanRevision())
	}

	cfgRec := httptest.NewRecorder()
	r.buildMux().ServeHTTP(cfgRec, httptest.NewRequest(http.MethodGet, "/config", nil))
	var shared sharedConfigResponse
	if err := json.Unmarshal(cfgRec.Body.Bytes(), &shared); err != nil || shared.AntiscanRevision != rev {
		t.Fatalf("/config must carry the revision: %+v err=%v", shared, err)
	}
}

func TestAntiscanSourceFailureKeepsLastGoodCopy(t *testing.T) {
	r, responses := antiscanFixture(t)
	now := time.Unix(1_800_000_000, 0)
	r.refreshAntiscan(context.Background(), now)
	rev := r.antiscanRevision()

	delete(responses, testScanSrcB)
	r.refreshAntiscan(context.Background(), now.Add(time.Hour))
	if r.antiscanRevision() != rev {
		t.Fatal("a failing source must not shrink the published list")
	}
	st := r.antiscanStatusSnapshot(r.cfg.Antiscan.Sources)
	if st.Sources[1].Error == "" || !strings.Contains(st.LastError, testScanSrcB) {
		t.Fatalf("failure must be visible in status: %+v", st)
	}
}

func TestAntiscanNoGeoMeansNoList(t *testing.T) {
	r, responses := antiscanFixture(t)
	delete(responses, testScanGeo)
	r.refreshAntiscan(context.Background(), time.Unix(1_800_000_000, 0))
	if r.antiscanRevision() != "" {
		t.Fatal("unfiltered abuse list must never be published")
	}
	if rec := antiscanGet(t, r); rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204 without a list, got %d", rec.Code)
	}
	if st := r.antiscanStatusSnapshot(nil); !strings.Contains(st.LastError, "RU") {
		t.Fatalf("status must explain the missing geo set: %+v", st)
	}
}

func TestAntiscanDisabledSignalsAgents(t *testing.T) {
	r, _ := antiscanFixture(t)
	r.refreshAntiscan(context.Background(), time.Unix(1_800_000_000, 0))
	off := false
	r.cfg.Antiscan.Enabled = &off
	r.refreshAntiscan(context.Background(), time.Unix(1_800_000_060, 0))
	if r.antiscanRevision() != machineapi.AntiscanDisabled {
		t.Fatalf("disabled config must publish %q, got %q", machineapi.AntiscanDisabled, r.antiscanRevision())
	}
	if rec := antiscanGet(t, r); rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204 when disabled, got %d", rec.Code)
	}
}

func TestAntiscanPanelEditHotApplies(t *testing.T) {
	r := newTestRegistryWithConfigFile(t)
	r.scan.kick = make(chan struct{}, 1)
	rec, _ := putConfig(t, r, `{"antiscan":{"enabled":true,"sources":[" https://x.example/l.txt ",""],"interval_min":15,"geo_url":""}}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT antiscan: %d %s", rec.Code, rec.Body.String())
	}
	if got := r.cfg.Antiscan.Sources; len(got) != 1 || got[0] != "https://x.example/l.txt" || r.cfg.Antiscan.IntervalMin != 15 || r.cfg.Antiscan.GeoURL == "" {
		t.Fatalf("antiscan section not applied: %+v", r.cfg.Antiscan)
	}
	select {
	case <-r.scan.kick:
	default:
		t.Fatal("saving antiscan settings must trigger an immediate rebuild")
	}
	for _, body := range []string{
		`{"antiscan":{"enabled":true,"sources":[],"interval_min":30,"geo_url":""}}`,
		`{"antiscan":{"enabled":true,"sources":["ftp://x"],"interval_min":30,"geo_url":""}}`,
		`{"antiscan":{"enabled":true,"sources":["https://x.example/l.txt"],"interval_min":1,"geo_url":""}}`,
	} {
		if rec, _ := putConfig(t, r, body, ""); rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid antiscan %s accepted: %d", body, rec.Code)
		}
	}
}
