package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudflare/cloudflare-go"
)

// fakeCF — in-memory реализация cfDNSAPI (V7.8): хранит записи зоны,
// честно делает list/create/update/delete. Ошибки можно инжектить флагами.
type fakeCF struct {
	mu      sync.Mutex
	records []cloudflare.DNSRecord
	nextID  int
	// инъекции ошибок (имена операций): "list", "create", "update", "delete"
	failOp map[string]bool
}

func newFakeCF() *fakeCF {
	return &fakeCF{failOp: map[string]bool{}}
}

func (f *fakeCF) fail(op string) error {
	if f.failOp[op] {
		return fmt.Errorf("injected %s failure", op)
	}
	return nil
}

func (f *fakeCF) ListDNSRecords(_ context.Context, _ *cloudflare.ResourceContainer, p cloudflare.ListDNSRecordsParams) ([]cloudflare.DNSRecord, *cloudflare.ResultInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("list"); err != nil {
		return nil, nil, err
	}
	out := make([]cloudflare.DNSRecord, 0, len(f.records))
	for _, rec := range f.records {
		if p.Name != "" && rec.Name != p.Name {
			continue
		}
		if p.Type != "" && !strings.EqualFold(rec.Type, p.Type) {
			continue
		}
		out = append(out, rec)
	}
	return out, &cloudflare.ResultInfo{}, nil
}

func (f *fakeCF) CreateDNSRecord(_ context.Context, _ *cloudflare.ResourceContainer, p cloudflare.CreateDNSRecordParams) (cloudflare.DNSRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("create"); err != nil {
		return cloudflare.DNSRecord{}, err
	}
	f.nextID++
	rec := cloudflare.DNSRecord{
		ID: "rec-" + strconv.Itoa(f.nextID), Type: p.Type, Name: p.Name,
		Content: p.Content, TTL: p.TTL, Proxied: p.Proxied,
	}
	f.records = append(f.records, rec)
	return rec, nil
}

func (f *fakeCF) UpdateDNSRecord(_ context.Context, _ *cloudflare.ResourceContainer, p cloudflare.UpdateDNSRecordParams) (cloudflare.DNSRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("update"); err != nil {
		return cloudflare.DNSRecord{}, err
	}
	for i := range f.records {
		if f.records[i].ID == p.ID {
			f.records[i].Type = p.Type
			f.records[i].Name = p.Name
			f.records[i].Content = p.Content
			f.records[i].TTL = p.TTL
			f.records[i].Proxied = p.Proxied
			return f.records[i], nil
		}
	}
	return cloudflare.DNSRecord{}, fmt.Errorf("record %q not found", p.ID)
}

func (f *fakeCF) DeleteDNSRecord(_ context.Context, _ *cloudflare.ResourceContainer, recordID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("delete"); err != nil {
		return err
	}
	for i := range f.records {
		if f.records[i].ID == recordID {
			f.records = append(f.records[:i], f.records[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("record %q not found", recordID)
}

// find — все записи имени (тип "" = любой).
func (f *fakeCF) find(name, typ string) []cloudflare.DNSRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []cloudflare.DNSRecord{}
	for _, rec := range f.records {
		if rec.Name == name && (typ == "" || strings.EqualFold(rec.Type, typ)) {
			out = append(out, rec)
		}
	}
	return out
}

func (f *fakeCF) seed(recs ...cloudflare.DNSRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, rec := range recs {
		f.nextID++
		rec.ID = "rec-" + strconv.Itoa(f.nextID)
		f.records = append(f.records, rec)
	}
}

// helpers: «здоровая нода» + применение changes как это делает selectionLoop.
func makeHealthyNow(r *Registry, id string) {
	c := r.state.Candidates[id]
	c.Healthy, c.GlobalpingOK, c.MetricsOK, c.MetricsHealthy = true, true, true, true
	c.LastReportAt = time.Now()
}

func applyChanges(t *testing.T, r *Registry, changes []domainChange) {
	t.Helper()
	for _, ch := range changes {
		if ch.ToID == "" {
			continue
		}
		r.applyDNSTarget(ch.Domain, ch.ToID, ch.ToIP)
	}
}

// V7.9: мастер пишется ТОЛЬКО A-записью на свой IP (selfmask-CNAME выпилен);
// заодно upsert сносит CNAME того же имени — наследие эксперимента V7.8
// (CNAME и A на одном имени несовместимы, такой хвост должен исчезать сам).
func TestMasterWritesAAndCleansStaleCNAME(t *testing.T) {
	fc := newFakeCF()
	r := newTestRegistry(t)
	r.cf = fc
	fc.seed(cloudflare.DNSRecord{Type: "CNAME", Name: "d1.example.com", Content: "mask.example.com"})

	r.register(registerRequest{NodeID: "A", IP: "1.1.1.1", NodeType: "mtproxyl"})
	makeHealthyNow(r, "A")
	applyChanges(t, r, r.evaluateAssignments(time.Now()))

	if r.state.Assignments["d1.example.com"] != "A" {
		t.Fatalf("A must be master, got %+v", r.state.Assignments)
	}
	recs := fc.find("d1.example.com", "")
	if len(recs) != 1 || recs[0].Type != "A" || recs[0].Content != "1.1.1.1" {
		t.Fatalf("master must yield single A record, got %+v", recs)
	}
	// событие — честное «A -> ip», счётчик инкрементирован
	if r.state.Counters.DNSUpdates != 1 {
		t.Fatalf("DNSUpdates: 1 expected, got %d", r.state.Counters.DNSUpdates)
	}
	found := false
	r.mu.RLock()
	for _, ev := range r.state.Events {
		if ev.Type == EventDNSUpdated && ev.Domain == "d1.example.com" && ev.Detail == "A -> 1.1.1.1" {
			found = true
		}
	}
	r.mu.RUnlock()
	if !found {
		t.Fatalf("dns_updated 'A -> 1.1.1.1' event expected, got %v", eventTypes(r))
	}
	// тип ноды сохранился
	if c := r.state.Candidates["A"]; c.NodeType != "mtproxyl" {
		t.Fatalf("node type must be stored, got %q", c.NodeType)
	}
}

// V7.9: перерегистрация с новым node_type обновляет его; пустой тип
// (старый агент) хранимое значение НЕ затирает.
func TestRegisterCarriesNodeType(t *testing.T) {
	r := newTestRegistry(t)
	r.register(registerRequest{NodeID: "A", IP: "1.1.1.1", NodeType: "classic"})
	if c := r.state.Candidates["A"]; c.NodeType != "classic" {
		t.Fatalf("classic expected, got %q", c.NodeType)
	}

	r.register(registerRequest{NodeID: "A", IP: "1.1.1.1"}) // старый агент без поля
	if c := r.state.Candidates["A"]; c.NodeType != "classic" {
		t.Fatalf("empty node_type must not clear stored value, got %q", c.NodeType)
	}

	r.register(registerRequest{NodeID: "A", IP: "1.1.1.1", NodeType: "meko"})
	if c := r.state.Candidates["A"]; c.NodeType != "meko" {
		t.Fatalf("type switch must be stored, got %q", c.NodeType)
	}
	found := false
	r.mu.RLock()
	for _, ev := range r.state.Events {
		if ev.Type == EventNodeRegistered && strings.Contains(ev.Detail, "type classic -> meko") {
			found = true
		}
	}
	r.mu.RUnlock()
	if !found {
		t.Fatal("re-register event must mention the type switch")
	}

	// /register через HTTP — тоже норм
	mux := r.buildMux()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(
		`{"node_id":"B","ip":"2.2.2.2","node_type":"mtproxyl"}`))
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("register B: %d %s", rec.Code, rec.Body.String())
	}
	if c := r.state.Candidates["B"]; c.NodeType != "mtproxyl" {
		t.Fatalf("B type: %q", c.NodeType)
	}
}

// V7.8: домен, удалённый из managed-списка, зачищается из Cloudflare — но
// ТОЛЬКО его A/AAAA/CNAME: TXT и чужие (никогда не управлявшиеся) записи зоны
// остаются нетронутыми. Ошибка CF → retry на следующем тике.
func TestSweepOrphansKeepsForeign(t *testing.T) {
	fc := newFakeCF()
	r := newTestRegistry(t)
	r.cf = fc
	// d1 был нашим managed-доменом, его вычеркнули из конфига (остался d2)
	r.cfg.Cloudflare.Domains = []string{"d2.example.com"}
	r.state.ManagedDomains = []string{"d1.example.com", "d2.example.com"}
	fc.seed(
		cloudflare.DNSRecord{Type: "A", Name: "d1.example.com", Content: "1.2.3.4"},
		cloudflare.DNSRecord{Type: "AAAA", Name: "d1.example.com", Content: "2001:db8::1"},
		cloudflare.DNSRecord{Type: "CNAME", Name: "d1.example.com", Content: "mask.example.com"},
		cloudflare.DNSRecord{Type: "TXT", Name: "d1.example.com", Content: "v=spf1 -all"},
		cloudflare.DNSRecord{Type: "A", Name: "foreign.example.com", Content: "8.8.8.8"},
	)

	r.sweepOrphans()

	// d1 очищен от A/AAAA/CNAME, TXT на месте
	left := fc.find("d1.example.com", "")
	if len(left) != 1 || left[0].Type != "TXT" {
		t.Fatalf("only TXT must survive the sweep, got %+v", left)
	}
	// чужой домен — нетронут
	if got := fc.find("foreign.example.com", ""); len(got) != 1 || got[0].Content != "8.8.8.8" {
		t.Fatalf("foreign record must be untouched, got %+v", got)
	}
	// d1 вычеркнут из managed; d2 (активен в конфиге) остался
	if len(r.state.ManagedDomains) != 1 || r.state.ManagedDomains[0] != "d2.example.com" {
		t.Fatalf("managed domains after sweep: %+v", r.state.ManagedDomains)
	}
	// событие + счётчики
	if r.state.Counters.DNSErrors != 0 {
		t.Fatalf("no DNS errors expected, got %d", r.state.Counters.DNSErrors)
	}
	found := false
	for _, ev := range r.state.Events {
		if ev.Type == EventDNSDeleted && ev.Domain == "d1.example.com" && strings.Contains(ev.Detail, "3 record(s)") {
			found = true
		}
	}
	if !found {
		t.Fatalf("dns_deleted event expected, got %v", eventTypes(r))
	}
	eventsAfter := len(r.state.Events)

	// повторный тик — no-op, nothing to sweep anymore
	r.sweepOrphans()
	if len(r.state.Events) != eventsAfter {
		t.Fatalf("second sweep must be a no-op, events grew: %d -> %d", eventsAfter, len(r.state.Events))
	}

	// ошибка CF: домен НЕ вычёркивается, будет retry на следующем тике
	r.cfg.Cloudflare.Domains = []string{"d2.example.com"}
	r.state.ManagedDomains = append(r.state.ManagedDomains, "d3.example.com")
	fc.seed(cloudflare.DNSRecord{Type: "A", Name: "d3.example.com", Content: "5.6.7.8"})
	fc.failOp["list"] = true
	r.sweepOrphans()
	if r.state.Counters.DNSErrors != 1 {
		t.Fatalf("one DNS error expected, got %d", r.state.Counters.DNSErrors)
	}
	found = false
	for _, d := range r.state.ManagedDomains {
		if d == "d3.example.com" {
			found = true
		}
	}
	if !found {
		t.Fatalf("failed orphan must stay in managed list for retry, got %+v", r.state.ManagedDomains)
	}
	if len(fc.find("d3.example.com", "")) == 0 {
		t.Fatal("d3 records must not be deleted on CF failure")
	}
	// CF «починили» — следующий тик дочищает
	fc.failOp["list"] = false
	r.sweepOrphans()
	if len(fc.find("d3.example.com", "")) != 0 {
		t.Fatal("d3 must be swept on retry")
	}
}
