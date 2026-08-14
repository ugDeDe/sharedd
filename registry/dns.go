package main

// DNS-операции с Cloudflare (V7.8+).
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

	"github.com/cloudflare/cloudflare-go"
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

// applyDNSTarget — upsert A-записи домена + учёт (счётчики, событие, персист).
// Cloudflare-вызов вне локов, учёт — под mu.
func (r *Registry) applyDNSTarget(domain, nodeID, ip string) {
	err := r.upsertARecord(domain, ip)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.state.Counters.DNSErrors++
		r.addEventLocked(Event{Type: EventDNSError, NodeID: nodeID, IP: ip, Domain: domain, Detail: err.Error()})
	} else {
		r.state.Counters.DNSUpdates++
		r.addEventLocked(Event{Type: EventDNSUpdated, NodeID: nodeID, IP: ip, Domain: domain, Detail: "A -> " + ip})
	}
	r.persistStateLocked()
}

// upsertARecord: домен = единственная A-запись на ip. Конфликтующие CNAME
// того же имени удаляются (наследие selfmask-эксперимента V7.8 — CNAME и A
// на одном имени несовместимы), дубли A сводятся к одной записи.
func (r *Registry) upsertARecord(domain, ip string) error {
	cf, zoneID, ttl, proxied := r.cfSnapshot()
	ctx := context.Background()
	rc := cloudflare.ZoneIdentifier(zoneID)

	recs, _, err := cf.ListDNSRecords(ctx, rc, cloudflare.ListDNSRecordsParams{Name: domain})
	if err != nil {
		log.Printf("cloudflare list error for %s: %v", domain, err)
		return fmt.Errorf("list: %w", err)
	}
	var firstA *cloudflare.DNSRecord
	for i := range recs {
		rec := recs[i]
		switch strings.ToUpper(rec.Type) {
		case "CNAME":
			log.Printf("dns %s: removing stale CNAME -> %s (writing A %s)", domain, rec.Content, ip)
			if derr := cf.DeleteDNSRecord(ctx, rc, rec.ID); derr != nil {
				return fmt.Errorf("delete stale CNAME: %w", derr)
			}
		case "A":
			if firstA == nil {
				firstA = &recs[i]
			} else {
				// дубликат A того же имени (был ручной вмеш?) — схлопываем
				if derr := cf.DeleteDNSRecord(ctx, rc, rec.ID); derr != nil {
					log.Printf("dns %s: failed to delete duplicate A -> %s: %v", domain, rec.Content, derr)
				}
			}
		}
	}
	if firstA != nil {
		_, err = cf.UpdateDNSRecord(ctx, rc, cloudflare.UpdateDNSRecordParams{
			ID: firstA.ID, Type: "A", Name: domain, Content: ip,
			TTL: ttl, Proxied: &proxied,
		})
	} else {
		_, err = cf.CreateDNSRecord(ctx, rc, cloudflare.CreateDNSRecordParams{
			Type: "A", Name: domain, Content: ip,
			TTL: ttl, Proxied: &proxied,
		})
	}
	if err != nil {
		log.Printf("cloudflare update error for %s: %v", domain, err)
		return err
	}
	log.Printf("dns updated: %s A -> %s", domain, ip)
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
		removed, err := r.sweepDNSOrphan(d)
		r.mu.Lock()
		if err != nil {
			r.state.Counters.DNSErrors++
			r.addEventLocked(Event{Type: EventDNSError, Domain: d, Detail: "orphan cleanup: " + err.Error()})
			r.persistStateLocked()
			r.mu.Unlock()
			continue
		}
		r.state.ManagedDomains = dropDomain(r.state.ManagedDomains, d)
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
