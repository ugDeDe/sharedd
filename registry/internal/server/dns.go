package server

// DNS-операции с Cloudflare.
//
// Managed-домен всегда ведёт A-записью на IP своей ноды-мастера. Плюс
// зачистка «сиротских» доменов: домен, вычеркнутый из managed-списка, теряет
// свои A/AAAA/CNAME-записи. ВАЖНО: чистим ТОЛЬКО домены, которыми регистратор
// реально управлял (state.ManagedDomains = бывшие домены конфига ∪ ключи
// назначений). Чужие записи зоны сюда не попадают никогда — в зоне живут и
// другие сервисы. TXT/NS/MX и прочие типы не трогаем даже у своих доменов.

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudflare/cloudflare-go"
)

const (
	dnsReconcileInterval = 5 * time.Second
	dnsRetryBase         = 5 * time.Second
	dnsRetryMax          = 5 * time.Minute
)

// cfDNSAPI — минимально используемый срез Cloudflare API; *cloudflare.API
// удовлетворяет интерфейсу напрямую, в тестах подсовывается фейк.
type cfDNSAPI interface {
	ListDNSRecords(ctx context.Context, rc *cloudflare.ResourceContainer, params cloudflare.ListDNSRecordsParams) ([]cloudflare.DNSRecord, *cloudflare.ResultInfo, error)
	CreateDNSRecord(ctx context.Context, rc *cloudflare.ResourceContainer, params cloudflare.CreateDNSRecordParams) (cloudflare.DNSRecord, error)
	UpdateDNSRecord(ctx context.Context, rc *cloudflare.ResourceContainer, params cloudflare.UpdateDNSRecordParams) (cloudflare.DNSRecord, error)
	DeleteDNSRecord(ctx context.Context, rc *cloudflare.ResourceContainer, recordID string) error
}

// cfSnapshot — клиент и параметры зоны под cfgMu (панель правит их на лету).
func (r *Registry) cfSnapshot() (cf cfDNSAPI, zoneID string, ttl int, proxied bool) {
	r.cfgMu.RLock()
	defer r.cfgMu.RUnlock()
	return r.cf, r.cfg.Cloudflare.ZoneID, r.cfg.Cloudflare.DNSTTL, r.cfg.Cloudflare.Proxied
}

// dnsMaster — один слот мастера домена: узел-источник и его IP для A-записи.
type dnsMaster struct {
	NodeID string
	IP     string
}

// equalStringSet — множества равны (порядок не важен, дубликатов нет).
func equalStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
		if seen[s] < 0 {
			return false
		}
	}
	return true
}

// enqueueDNSDesiredLocked — желаемый набор целей домена: для A — IP всех
// мастеров (порядок не важен), для CNAME — ровно один элемент. Набор не
// изменился (тот же тип + то же множество) — false, Generation не растёт.
func (r *Registry) enqueueDNSDesiredLocked(domain, typ string, targets, nodes []string) bool {
	if r.state.DNSOperations == nil {
		r.state.DNSOperations = make(map[string]*DNSOperation)
	}
	typ = strings.ToUpper(strings.TrimSpace(typ))
	op := r.state.DNSOperations[domain]
	if op == nil {
		op = &DNSOperation{}
		r.state.DNSOperations[domain] = op
	}
	if op.DesiredType == typ && equalStringSet(op.DesiredTargets, targets) {
		return false
	}
	op.DesiredType = typ
	op.DesiredTargets = append([]string(nil), targets...)
	op.DesiredNodes = append([]string(nil), nodes...)
	op.Attempts, op.NextAttempt, op.LastError = 0, time.Time{}, ""
	op.Generation++
	return true
}

// enqueueDNSDesiredFromAssignmentsLocked — желаемый DNS-набор домена из его
// слотов назначения (A) либо из CNames (свёрнутый СРМД домен). Домен с
// ПУСТЫМ списком слотов DNS не трогает: последняя желаемая запись остаётся —
// инвариант «пустая очередь не снимает назначения». Возвращает true, если
// желаемое изменилось. Под r.mu (write).
func (r *Registry) enqueueDNSDesiredFromAssignmentsLocked(d string) bool {
	if cn := r.state.SRMD.CNames[d]; cn != "" {
		return r.enqueueDNSDesiredLocked(d, "CNAME", []string{cn}, nil)
	}
	nodes := r.state.Assignments[d]
	if len(nodes) == 0 {
		return false
	}
	masters := make([]dnsMaster, 0, len(nodes))
	for _, id := range nodes {
		if c := r.state.Candidates[id]; c != nil && c.IP != "" {
			masters = append(masters, dnsMaster{NodeID: id, IP: c.IP})
		}
	}
	if len(masters) == 0 {
		return false
	}
	targets := make([]string, 0, len(masters))
	ids := make([]string, 0, len(masters))
	for _, m := range masters {
		targets = append(targets, m.IP)
		ids = append(ids, m.NodeID)
	}
	return r.enqueueDNSDesiredLocked(d, "A", targets, ids)
}

func dnsRetryDelay(attempts int) time.Duration {
	d := dnsRetryBase
	for i := 1; i < attempts && d < dnsRetryMax; i++ {
		d *= 2
	}
	if d > dnsRetryMax {
		return dnsRetryMax
	}
	return d
}

func (r *Registry) reconcileDNSDomain(domain string, force bool) error {
	now := time.Now()
	r.mu.Lock()
	if r.dnsInFlight == nil {
		r.dnsInFlight = make(map[string]bool)
	}
	op := r.state.DNSOperations[domain]
	if op == nil || !op.Drifted() || (!force && now.Before(op.NextAttempt)) || r.dnsInFlight[domain] || !r.domainInConfigLocked(domain) {
		r.mu.Unlock()
		return nil
	}
	r.dnsInFlight[domain] = true
	typ := op.DesiredType
	targets := append([]string(nil), op.DesiredTargets...)
	nodes := append([]string(nil), op.DesiredNodes...)
	generation := op.Generation
	op.Attempts++
	r.persistStateLocked()
	r.mu.Unlock()

	var err error
	switch typ {
	case "A":
		err = r.reconcileARecord(domain, targets)
	case "CNAME":
		if len(targets) != 1 {
			err = fmt.Errorf("CNAME needs exactly one target, got %d", len(targets))
		} else {
			err = r.upsertCNAMERecord(domain, targets[0])
		}
	default:
		err = fmt.Errorf("unsupported desired DNS type %q", typ)
	}

	now = time.Now()
	r.mu.Lock()
	delete(r.dnsInFlight, domain)
	op = r.state.DNSOperations[domain]
	if op == nil {
		r.mu.Unlock()
		return err
	}
	if err != nil {
		r.state.Counters.DNSErrors++
		nodeID, ip := firstNonEmpty(nodes), ""
		if typ == "A" && len(targets) > 0 {
			ip = targets[0]
		}
		r.addEventLocked(Event{Type: EventDNSError, NodeID: nodeID, IP: ip, Domain: domain, Detail: err.Error()})
		if op.Generation == generation {
			op.LastError = err.Error()
			op.NextAttempt = now.Add(dnsRetryDelay(op.Attempts))
		}
	} else {
		op.AppliedType, op.LastSuccess = typ, now
		op.AppliedTargets = append([]string(nil), targets...)
		r.state.Counters.DNSUpdates++
		// один цикл событий на домен; detail перечисляет весь набор A
		detail, ip := typ+" -> "+strings.Join(targets, ", "), ""
		if typ == "A" && len(targets) > 0 {
			ip = targets[0]
		} else if typ != "A" {
			detail += " (СРМД)"
		}
		nodeID := firstNonEmpty(nodes)
		r.addEventLocked(Event{Type: EventDNSUpdated, NodeID: nodeID, IP: ip, Domain: domain, Detail: detail})
		if op.Generation == generation {
			op.LastError, op.NextAttempt = "", time.Time{}
		}
	}
	r.persistStateLocked()
	r.mu.Unlock()
	return err
}

// firstNonEmpty — первый непустой элемент ("" если нет).
func firstNonEmpty(list []string) string {
	for _, s := range list {
		if s != "" {
			return s
		}
	}
	return ""
}

func (r *Registry) reconcileDNSDue() {
	r.mu.RLock()
	domains := make([]string, 0, len(r.state.DNSOperations))
	for domain := range r.state.DNSOperations {
		domains = append(domains, domain)
	}
	r.mu.RUnlock()
	var wg sync.WaitGroup
	for _, domain := range domains {
		wg.Add(1)
		go func(d string) {
			defer wg.Done()
			_ = r.reconcileDNSDomain(d, false)
		}(domain)
	}
	wg.Wait()
}

func (r *Registry) dnsReconcileLoop() {
	r.reconcileDNSDue()
	ticker := time.NewTicker(dnsReconcileInterval)
	defer ticker.Stop()
	for range ticker.C {
		r.reconcileDNSDue()
	}
}

func (r *Registry) recoverDNSDesiredLocked() {
	for domain, target := range r.state.SRMD.CNames {
		r.enqueueDNSDesiredLocked(domain, "CNAME", []string{target}, nil)
	}
	for domain := range r.state.Assignments {
		if r.state.SRMD.CNames[domain] != "" {
			continue
		}
		r.enqueueDNSDesiredFromAssignmentsLocked(domain)
	}
}

func (r *Registry) domainInConfigLocked(domain string) bool {
	r.cfgMu.RLock()
	defer r.cfgMu.RUnlock()
	for _, d := range r.cfg.Cloudflare.Domains {
		if strings.TrimSpace(d) == domain {
			return true
		}
	}
	return false
}

// reconcileARecord — сверка ПОЛНОГО набора A-записей домена с желаемым
// (мульти-мастер DNS round-robin; порядок не важен): создать недостающие A,
// удалить лишние A, совпадающие не трогать. Конфликтующие CNAME того же
// имени удаляются (наследие selfmask-эксперимента — CNAME и A на одном
// имени несовместимы). Набор из одного IP — частный случай (прежнее
// поведение: домен = единственная A-запись).
func (r *Registry) reconcileARecord(domain string, ips []string) error {
	if len(ips) == 0 {
		return fmt.Errorf("empty desired A set for %s", domain)
	}
	cf, zoneID, _, _ := r.cfSnapshot()
	ctx := context.Background()
	rc := cloudflare.ZoneIdentifier(zoneID)

	recs, _, err := cf.ListDNSRecords(ctx, rc, cloudflare.ListDNSRecordsParams{Name: domain})
	if err != nil {
		log.Printf("cloudflare list error for %s: %v", domain, err)
		return fmt.Errorf("list: %w", err)
	}
	want := make(map[string]bool, len(ips))
	for _, ip := range ips {
		if ip != "" {
			want[ip] = true
		}
	}
	if len(want) == 0 {
		return fmt.Errorf("empty desired A set for %s", domain)
	}
	created := 0
	for i := range recs {
		rec := recs[i]
		switch strings.ToUpper(rec.Type) {
		case "CNAME":
			log.Printf("dns %s: removing stale CNAME -> %s (writing A %s)", domain, rec.Content, strings.Join(ips, ", "))
			if derr := cf.DeleteDNSRecord(ctx, rc, rec.ID); derr != nil {
				return fmt.Errorf("delete stale CNAME: %w", derr)
			}
		case "A":
			if want[rec.Content] {
				delete(want, rec.Content) // совпадающая A — не трогаем
			} else {
				// лишняя A (мастер сдал слот) или дубль ручного вмешательства
				log.Printf("dns %s: removing extra A -> %s", domain, rec.Content)
				if derr := cf.DeleteDNSRecord(ctx, rc, rec.ID); derr != nil {
					return fmt.Errorf("delete extra A: %w", derr)
				}
			}
		}
	}
	r.cfgMu.RLock()
	ttl, proxied := r.cfg.Cloudflare.DNSTTL, r.cfg.Cloudflare.Proxied
	r.cfgMu.RUnlock()
	for ip := range want {
		if _, cerr := cf.CreateDNSRecord(ctx, rc, cloudflare.CreateDNSRecordParams{
			Type: "A", Name: domain, Content: ip,
			TTL: ttl, Proxied: &proxied,
		}); cerr != nil {
			log.Printf("cloudflare create error for %s: %v", domain, cerr)
			return cerr
		}
		created++
	}
	if created > 0 {
		log.Printf("dns updated: %s A -> %s", domain, strings.Join(ips, ", "))
	}
	return nil
}

// UpsertCNAMERecord — домен = единственная CNAME-запись на target (
// СРМД сворачивает лишние домены на выживших). Конфликтующие A/AAAA того же
// имени удаляются (до сворачивания домен жил A-записью на IP мастера),
// дубли CNAME схлопываются в одну. Разворачивание обратно в A делает
// reconcileARecord — он, в свою очередь, вычищает CNAME.
func (r *Registry) upsertCNAMERecord(domain, target string) error {
	cf, zoneID, ttl, proxied := r.cfSnapshot()
	ctx := context.Background()
	rc := cloudflare.ZoneIdentifier(zoneID)

	recs, _, err := cf.ListDNSRecords(ctx, rc, cloudflare.ListDNSRecordsParams{Name: domain})
	if err != nil {
		log.Printf("cloudflare list error for %s: %v", domain, err)
		return fmt.Errorf("list: %w", err)
	}
	var firstCNAME *cloudflare.DNSRecord
	for i := range recs {
		rec := recs[i]
		switch strings.ToUpper(rec.Type) {
		case "A", "AAAA":
			log.Printf("dns %s: removing stale %s -> %s (writing CNAME %s)", domain, rec.Type, rec.Content, target)
			if derr := cf.DeleteDNSRecord(ctx, rc, rec.ID); derr != nil {
				return fmt.Errorf("delete stale %s: %w", rec.Type, derr)
			}
		case "CNAME":
			if firstCNAME == nil {
				firstCNAME = &recs[i]
			} else if derr := cf.DeleteDNSRecord(ctx, rc, rec.ID); derr != nil {
				log.Printf("dns %s: failed to delete duplicate CNAME -> %s: %v", domain, rec.Content, derr)
			}
		}
	}
	if firstCNAME != nil {
		_, err = cf.UpdateDNSRecord(ctx, rc, cloudflare.UpdateDNSRecordParams{
			ID: firstCNAME.ID, Type: "CNAME", Name: domain, Content: target,
			TTL: ttl, Proxied: &proxied,
		})
	} else {
		_, err = cf.CreateDNSRecord(ctx, rc, cloudflare.CreateDNSRecordParams{
			Type: "CNAME", Name: domain, Content: target,
			TTL: ttl, Proxied: &proxied,
		})
	}
	if err != nil {
		log.Printf("cloudflare update error for %s: %v", domain, err)
		return err
	}
	log.Printf("dns updated: %s CNAME -> %s", domain, target)
	return nil
}

// ── «сиротские» домены (выведены из managed-списка) ─────────────────────

// reconcileManagedDomainsLocked пересобирает множество доменов, о которых мы
// ЗНАЕМ, что управляли: текущий конфиг ∪ ключи назначений ∪ прошлое значение.
// Ровно по этому множеству позже вычисляются сироты — чужие домены зоны
// сюда не попадают в принципе. Возвращает true, если множество изменилось.
// ВЫЗЫВАТЬ под r.mu (write-lock; порядок локов mu→cfgMu соблюдён).
func (r *Registry) reconcileManagedDomainsLocked() bool {
	r.cfgMu.RLock()
	cfgDomains := r.cfg.Cloudflare.Domains
	r.cfgMu.RUnlock()

	set := make(map[string]bool, len(r.state.ManagedDomains)+len(cfgDomains)+len(r.state.Assignments))
	for _, d := range r.state.ManagedDomains {
		if d = strings.TrimSpace(d); d != "" {
			set[d] = true
		}
	}
	for _, d := range cfgDomains {
		if d = strings.TrimSpace(d); d != "" {
			set[d] = true
		}
	}
	for d := range r.state.Assignments {
		set[d] = true
	}
	out := make([]string, 0, len(set))
	for d := range set {
		out = append(out, d)
	}
	sort.Strings(out)
	changed := !equalStrings(out, r.state.ManagedDomains)
	r.state.ManagedDomains = out
	return changed
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// computeDNSOrphansLocked — домены, которыми управляли, но которых больше
// нет в конфиге: их записи подлежат зачистке. Под r.mu.
func (r *Registry) computeDNSOrphansLocked() []string {
	r.cfgMu.RLock()
	inCfg := make(map[string]bool, len(r.cfg.Cloudflare.Domains))
	for _, d := range r.cfg.Cloudflare.Domains {
		inCfg[strings.TrimSpace(d)] = true
	}
	r.cfgMu.RUnlock()

	orphans := make([]string, 0, 2)
	for _, d := range r.state.ManagedDomains {
		if !inCfg[d] {
			orphans = append(orphans, d)
		}
	}
	return orphans
}

// sweepOrphans — зачистка доменов, удалённых из managed-списка: вызывается
// каждый тик селекции и сразу после правки cloudflare-секции в панели.
// Успешно очищенный домен (или домен без записей) вычёркивается из
// ManagedDomains; при ошибке остаётся на retry следующего тика.
func (r *Registry) sweepOrphans() {
	r.mu.Lock()
	changed := r.reconcileManagedDomainsLocked()
	orphans := r.computeDNSOrphansLocked()
	if changed && len(orphans) == 0 {
		r.persistStateLocked()
	}
	r.mu.Unlock()

	for _, d := range orphans {
		r.mu.Lock()
		if r.dnsInFlight == nil {
			r.dnsInFlight = make(map[string]bool)
		}
		if r.dnsInFlight[d] || r.domainInConfigLocked(d) {
			r.mu.Unlock()
			continue
		}
		r.dnsInFlight[d] = true
		r.mu.Unlock()
		removed, err := r.sweepDNSOrphan(d)
		r.mu.Lock()
		delete(r.dnsInFlight, d)
		if err != nil {
			r.state.Counters.DNSErrors++
			r.addEventLocked(Event{Type: EventDNSError, Domain: d, Detail: "orphan cleanup: " + err.Error()})
			r.persistStateLocked()
			r.mu.Unlock()
			continue
		}
		// Config may have been updated while Cloudflare was in flight.
		if r.domainInConfigLocked(d) {
			r.recoverDNSDesiredLocked()
			if op := r.state.DNSOperations[d]; op != nil && removed > 0 {
				op.AppliedType, op.AppliedTargets = "", nil
				op.NextAttempt = time.Time{}
			}
			r.persistStateLocked()
			r.mu.Unlock()
			if removed > 0 {
				_ = r.reconcileDNSDomain(d, true)
			}
			continue
		}
		r.state.ManagedDomains = dropDomain(r.state.ManagedDomains, d)
		delete(r.state.DNSOperations, d)
		if removed > 0 {
			r.addEventLocked(Event{
				Type: EventDNSDeleted, Domain: d,
				Detail: fmt.Sprintf("domain removed from managed list — deleted %d record(s)", removed),
			})
		}
		r.persistStateLocked()
		r.mu.Unlock()
	}
}

// sweepDNSOrphan удаляет ТОЛЬКО A/AAAA/CNAME этого конкретного имени —
// TXT/MX/NS и прочие записи (почта, верификации) остаются нетронутыми.
// Возвращает число удалённых записей.
func (r *Registry) sweepDNSOrphan(domain string) (int, error) {
	r.mu.RLock()
	managedAgain := r.domainInConfigLocked(domain)
	r.mu.RUnlock()
	if managedAgain {
		return 0, nil
	}
	cf, zoneID, _, _ := r.cfSnapshot()
	ctx := context.Background()
	rc := cloudflare.ZoneIdentifier(zoneID)

	recs, _, err := cf.ListDNSRecords(ctx, rc, cloudflare.ListDNSRecordsParams{Name: domain})
	if err != nil {
		log.Printf("cloudflare list error (orphan %s): %v", domain, err)
		return 0, fmt.Errorf("list: %w", err)
	}
	removed := 0
	for i := range recs {
		rec := recs[i]
		switch strings.ToUpper(rec.Type) {
		case "A", "AAAA", "CNAME":
			r.mu.RLock()
			managedAgain = r.domainInConfigLocked(domain)
			r.mu.RUnlock()
			if managedAgain {
				return removed, nil
			}
			if derr := cf.DeleteDNSRecord(ctx, rc, rec.ID); derr != nil {
				return removed, fmt.Errorf("delete %s: %w", rec.Type, derr)
			}
			removed++
			log.Printf("dns orphan %s: deleted %s -> %s", domain, rec.Type, rec.Content)
		default:
			log.Printf("dns orphan %s: keeping %s record (not proxy-managed)", domain, rec.Type)
		}
	}
	if removed == 0 {
		log.Printf("dns orphan %s: nothing to delete", domain)
	}
	return removed, nil
}
