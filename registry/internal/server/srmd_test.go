package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// addHealthyNode — полностью здоровая нода в пуле (сразу входит в очередь).
func addHealthyNode(t *testing.T, r *Registry, id, ip string) *Candidate {
	t.Helper()
	now := time.Now()
	c := &Candidate{
		NodeID:         id,
		IP:             ip,
		RegisteredAt:   now,
		LastHeartbeat:  now,
		Healthy:        true,
		GlobalpingOK:   true,
		MetricsHealthy: true,
		LastReportAt:   now,
	}
	r.state.Candidates[id] = c
	return c
}

// srmdTestRegistry — пул с основным доменом shared.ddproxy.xyz и включённой
// СРМД (если не сказано иного). configPath — временный файл: СРМД персистит
// созданные домены обратно в TOML.
func srmdTestRegistry(t *testing.T) *Registry {
	t.Helper()
	r := newTestRegistry(t)
	r.cfg.Cloudflare.Domains = []string{"shared.ddproxy.xyz"}
	r.cfg.SRMD.MaxNodesPerDomain = 3
	r.cfg.ConfigPath = filepath.Join(t.TempDir(), "registry.toml")
	enabled := true
	r.cfg.SRMD.Enabled = &enabled
	r.state.Assignments = make(map[string]AssignmentSlots)
	r.state.AssignmentsSince = make(map[string]SinceSlots)
	return r
}

// tickSRMD — srmdStableTicks+1 проходов селекции: условие масштабирования
// успевает «отстояться» и действие точно случается.
func tickSRMD(t *testing.T, r *Registry) {
	t.Helper()
	for i := 0; i < srmdStableTicks+1; i++ {
		r.evaluateAssignments(time.Now())
	}
}

func TestSRMDRequiredDomains(t *testing.T) {
	cases := []struct{ queue, max, want int }{
		{0, 5, 1},  // пусто — хотя бы основной домен
		{1, 5, 1},  // 1..max нод = один домен
		{5, 5, 1},  // ровно лимит
		{6, 5, 2},  // ТЗ: нод 6, лимит 5 — нужен второй домен
		{10, 3, 4}, // пример 1: 10 нод / 3 на домен = 4 домена
		{4, 3, 2},  // пример 1 после падения пула: 4 ноды = 2 домена
		{10, 0, 2}, // мусорный лимит → дефолт 5
	}
	for _, c := range cases {
		if got := srmdRequiredDomains(c.queue, c.max, 1); got != c.want {
			t.Fatalf("srmdRequiredDomains(%d, %d, M=1) = %d, want %d", c.queue, c.max, got, c.want)
		}
	}
}

// Обязательный инвариант мульти-мастера: ёмкость домена масштабируется
// множителем мастеров, поэтому M=2 удваивает число нод под одним доменом.
// srmdRequiredDomains(queue, maxPerDomain=5, M=2) == ceil(queue/10).
func TestSRMDRequiredDomainsScalesWithMasters(t *testing.T) {
	cases := []struct {
		queue, maxPerDomain, masters, want int
	}{
		{10, 5, 2, 1}, // ровно ёмкость 10
		{11, 5, 2, 2}, // ceil(11/10)
		{20, 5, 2, 2}, // ceil(20/10)
		{21, 5, 2, 3}, // ceil(21/10)
		{6, 5, 2, 1},  // M=2: 6 нод всё ещё в одном домене (было бы 2 при M=1)
		{10, 5, 1, 2}, // M=1 — прежнее поведение
		{6, 5, 1, 2},  // M=1: нод 6, лимит 5 → 2 домена
		{0, 5, 2, 1},  // пусто — хотя бы один домен
		{5, 5, 8, 1},  // M=8: ёмкость 40
		{41, 5, 8, 2}, // ceil(41/40)
	}
	for _, c := range cases {
		got := srmdRequiredDomains(c.queue, c.maxPerDomain, c.masters)
		if got != c.want {
			t.Fatalf("srmdRequiredDomains(queue=%d, max=%d, M=%d) = %d, want %d",
				c.queue, c.maxPerDomain, c.masters, got, c.want)
		}
	}
	// прямая формулировка из спеки
	if got := srmdRequiredDomains(37, 5, 2); got != 4 { // ceil(37/10)
		t.Fatalf("srmdRequiredDomains(37,5,2) = %d, want ceil(37/10)=4", got)
	}
}

func TestSRMDSplitBase(t *testing.T) {
	prefix, suffix, ok := srmdSplitBase("shared.ddproxy.xyz")
	if !ok || prefix != "shared" || suffix != ".ddproxy.xyz" {
		t.Fatalf("bad split: %q %q %v", prefix, suffix, ok)
	}
	if _, _, ok := srmdSplitBase("nakedname"); ok {
		t.Fatal("name without dot must not split")
	}
	if _, _, ok := srmdSplitBase(".leading.dot"); ok {
		t.Fatal("empty prefix must not split")
	}
}

func TestSRMDNextNameSkipsTaken(t *testing.T) {
	r := srmdTestRegistry(t)
	// shared1 уже занят (ручной) — инкремент перескакивает на shared2
	r.cfg.Cloudflare.Domains = append(r.cfg.Cloudflare.Domains, "shared1.ddproxy.xyz")
	if name := r.srmdNextNameLocked("shared", ".ddproxy.xyz"); name != "shared2.ddproxy.xyz" {
		t.Fatalf("want shared2.ddproxy.xyz, got %s", name)
	}
}

func TestSRMDCreatesDomainWhenQueueGrows(t *testing.T) {
	r := srmdTestRegistry(t) // лимит 3 ноды/домен
	r.cfg.SRMD.MaxNodesPerDomain = 2
	// 3 здоровые ноды при одном домене: ceil(3/2)=2 — СРМД обязана создать второй
	addHealthyNode(t, r, "node-a", "10.0.0.1")
	addHealthyNode(t, r, "node-b", "10.0.0.2")
	addHealthyNode(t, r, "node-c", "10.0.0.3")

	tickSRMD(t, r)

	if len(r.cfg.Cloudflare.Domains) != 2 {
		t.Fatalf("want 2 managed domains, got %v", r.cfg.Cloudflare.Domains)
	}
	created := r.cfg.Cloudflare.Domains[1]
	if created != "shared1.ddproxy.xyz" {
		t.Fatalf("orphan domain must be shared1.ddproxy.xyz, got %s", created)
	}
	if len(r.state.SRMD.Created) != 1 || r.state.SRMD.Created[0] != created {
		t.Fatalf("SRMD must remember created domain: %v", r.state.SRMD.Created)
	}
	if r.state.Counters.SRMDCreated != 1 {
		t.Fatalf("counter: %v", r.state.Counters.SRMDCreated)
	}
	// созданному домену сразу достался мастер (нод хватает)
	if len(r.state.Assignments[created]) == 0 {
		t.Fatal("created domain must get a master on the same tick")
	}
	// событие в журнале
	found := false
	for _, ev := range r.state.Events {
		if ev.Type == EventSRMDDomainCreated && ev.Domain == created {
			found = true
		}
	}
	if !found {
		t.Fatal("srmd_domain_created event missing")
	}
}

func TestSRMDDisabledDoesNotCreateAndAlerts(t *testing.T) {
	r := srmdTestRegistry(t)
	r.cfg.SRMD.Enabled = nil // выключено по стандарту
	r.cfg.SRMD.MaxNodesPerDomain = 5
	// 6 нод при одном домене и лимите 5 — очередь переполнена
	for i, ip := range []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5", "10.0.0.6"} {
		addHealthyNode(t, r, "node-"+string(rune('a'+i)), ip)
	}

	tickSRMD(t, r)

	if len(r.cfg.Cloudflare.Domains) != 1 {
		t.Fatalf("disabled SRMD must not create domains: %v", r.cfg.Cloudflare.Domains)
	}
	ov := r.buildOverview()
	if ov.SRMD == nil || ov.SRMD.Alert == "" {
		t.Fatalf("overview must carry the «нод слишком много в очереди» alert: %+v", ov.SRMD)
	}
}

func TestSRMDFoldsDomainsBalancingClients(t *testing.T) {
	// Пример 1 из ТЗ: лимит 3, нод 10 → 4 домена; нод стало 4 → доменов 2.
	r := srmdTestRegistry(t)
	r.cfg.Cloudflare.Domains = []string{
		"shared.ddproxy.xyz", "shared1.ddproxy.xyz",
		"shared2.ddproxy.xyz", "shared3.ddproxy.xyz",
	}
	r.state.SRMD.Created = []string{"shared1.ddproxy.xyz", "shared2.ddproxy.xyz", "shared3.ddproxy.xyz"}
	r.state.SRMD.DomainClients = map[string]int{
		"shared.ddproxy.xyz":  1500,
		"shared1.ddproxy.xyz": 380,
		"shared2.ddproxy.xyz": 1000,
		"shared3.ddproxy.xyz": 1400,
	}
	// эмуляция живых мастеров на всех четырёх доменах
	for i, d := range r.cfg.Cloudflare.Domains {
		id := "node-" + string(rune('a'+i))
		addHealthyNode(t, r, id, "10.0.0."+string(rune('1'+i)))
		r.state.Assignments[d] = AssignmentSlots{id}
	}

	// пул сжался до 4 нод: убираем 6 из 10 (остались a..d)
	tickSRMD(t, r)

	// ceil(4/3)=2 домена: лишние shared2 и shared3 свёрнуты, причём
	// балансировкой: крупный shared3 уходит наименее загруженному shared1,
	// shared2 — основному shared.
	want := map[string]string{
		"shared3.ddproxy.xyz": "shared1.ddproxy.xyz",
		"shared2.ddproxy.xyz": "shared.ddproxy.xyz",
	}
	if len(r.state.SRMD.CNames) != len(want) {
		t.Fatalf("cnames: %v", r.state.SRMD.CNames)
	}
	for d, target := range want {
		if r.state.SRMD.CNames[d] != target {
			t.Fatalf("fold %s: want CNAME -> %s, got %v", d, target, r.state.SRMD.CNames)
		}
		if len(r.state.Assignments[d]) != 0 {
			t.Fatalf("folded domain %s must not keep a master", d)
		}
	}
	if got := r.state.SRMD.DomainClients["shared.ddproxy.xyz"]; got != 2500 {
		t.Fatalf("shared clients 1500+1000: got %d", got)
	}
	if got := r.state.SRMD.DomainClients["shared1.ddproxy.xyz"]; got != 1780 {
		t.Fatalf("shared1 clients 380+1400: got %d", got)
	}
	if len(r.srmdPending) != 2 {
		t.Fatalf("two CNAME DNS actions expected, got %v", r.srmdPending)
	}
	// свёрнутые домены больше не получают мастеров
	for i := 0; i < 3; i++ {
		r.evaluateAssignments(time.Now())
	}
	for d := range want {
		if len(r.state.Assignments[d]) != 0 {
			t.Fatalf("folded domain %s got a master after re-evaluation", d)
		}
	}
}

func TestSRMDUnfoldsFoldedBeforeCreatingNew(t *testing.T) {
	r := srmdTestRegistry(t)
	// раньше было 2 домена, пул падал — shared1 свёрнут в CNAME
	r.cfg.Cloudflare.Domains = []string{"shared.ddproxy.xyz", "shared1.ddproxy.xyz"}
	r.state.SRMD.Created = []string{"shared1.ddproxy.xyz"}
	r.state.SRMD.CNames = map[string]string{"shared1.ddproxy.xyz": "shared.ddproxy.xyz"}
	r.cfg.SRMD.MaxNodesPerDomain = 2
	// очередь выросла: ceil(3/2)=2 — нужен ВТОРОЙ домен, и свободный уже есть
	addHealthyNode(t, r, "node-a", "10.0.0.1")
	addHealthyNode(t, r, "node-b", "10.0.0.2")
	addHealthyNode(t, r, "node-c", "10.0.0.3")

	tickSRMD(t, r)

	if len(r.state.SRMD.CNames) != 0 {
		t.Fatalf("folded domain must be unfolded instead of creating new: %v", r.state.SRMD.CNames)
	}
	if len(r.cfg.Cloudflare.Domains) != 2 {
		t.Fatalf("no new domain expected: %v", r.cfg.Cloudflare.Domains)
	}
	if r.state.Counters.SRMDUnfolded != 1 {
		t.Fatalf("unfold counter: %v", r.state.Counters.SRMDUnfolded)
	}
	// развёрнутый домен снова получил мастера
	if len(r.state.Assignments["shared1.ddproxy.xyz"]) == 0 {
		t.Fatal("unfolded domain must rejoin master rotation")
	}
}

func TestSRMDKeepsLastClientsWithoutMaster(t *testing.T) {
	// «даже если активного мастера на домене сейчас нет — хранить последнее
	// значение активных»: мастер пропал, число клиентов не стирается.
	r := srmdTestRegistry(t)
	r.state.SRMD.DomainClients = map[string]int{"shared.ddproxy.xyz": 777}
	r.evaluateAssignments(time.Now())
	if r.state.SRMD.DomainClients["shared.ddproxy.xyz"] != 777 {
		t.Fatalf("last known clients must survive master loss: %v", r.state.SRMD.DomainClients)
	}
}

func TestSRMDClientTableFromLiveMasters(t *testing.T) {
	// таблица «домен | активные пользователи» снимается с мастеров по общему
	// секрету (агрегат uniqueIPsMetric в снапшоте ноды)
	r := srmdTestRegistry(t)
	c := addHealthyNode(t, r, "node-a", "10.0.0.1")
	c.MetricsSnapshot = map[string]float64{uniqueIPsMetric: 399}
	r.evaluateAssignments(time.Now()) // домен получает мастера
	r.evaluateAssignments(time.Now()) // снимаем клиенты
	if got := r.state.SRMD.DomainClients["shared.ddproxy.xyz"]; got != 399 {
		t.Fatalf("domain clients must come from master metrics: %v", r.state.SRMD.DomainClients)
	}
	ov := r.buildOverview()
	if ov.SRMD == nil || len(ov.SRMD.Domains) != 1 {
		t.Fatalf("overview SRMD block: %+v", ov.SRMD)
	}
	row := ov.SRMD.Domains[0]
	if row.Clients == nil || *row.Clients != 399 || !row.Fresh || !row.Base {
		t.Fatalf("srmd domain row wrong: %+v", row)
	}
}

func TestSRMDFoldGreedyMatchesExample2(t *testing.T) {
	// Пример 2 из ТЗ: оба мелких домена уходят shared1 (жадная балансировка
	// по текущей нагрузке, а не «всё в один»).
	r := srmdTestRegistry(t)
	r.cfg.Cloudflare.Domains = []string{
		"shared.ddproxy.xyz", "shared1.ddproxy.xyz",
		"shared2.ddproxy.xyz", "shared3.ddproxy.xyz",
	}
	r.state.SRMD.Created = []string{"shared1.ddproxy.xyz", "shared2.ddproxy.xyz", "shared3.ddproxy.xyz"}
	r.state.SRMD.DomainClients = map[string]int{
		"shared.ddproxy.xyz":  1500,
		"shared1.ddproxy.xyz": 380,
		"shared2.ddproxy.xyz": 310,
		"shared3.ddproxy.xyz": 470,
	}
	for i := 0; i < 4; i++ {
		addHealthyNode(t, r, "node-"+string(rune('a'+i)), "10.0.0."+string(rune('1'+i)))
	}

	tickSRMD(t, r)

	if r.state.SRMD.CNames["shared3.ddproxy.xyz"] != "shared1.ddproxy.xyz" ||
		r.state.SRMD.CNames["shared2.ddproxy.xyz"] != "shared1.ddproxy.xyz" {
		t.Fatalf("example 2 fold targets wrong: %v", r.state.SRMD.CNames)
	}
	if got := r.state.SRMD.DomainClients["shared.ddproxy.xyz"]; got != 1500 {
		t.Fatalf("shared stays 1500: got %d", got)
	}
	if got := r.state.SRMD.DomainClients["shared1.ddproxy.xyz"]; got != 1160 {
		t.Fatalf("shared1 must end at 380+470+310=1160: got %d", got)
	}
}

// Ручной домен можно насильно отдать под контроль СРМД (тогда он
// сворачивается при сжатии пула), домен СРМД — вернуть в ручной режим
// (сворачиваться перестаёт; свёрнутый при этом разворачивается).
func TestSRMDTakeAndReleaseDomain(t *testing.T) {
	r := srmdTestRegistry(t)
	r.cfg.PanelEnabled = true // маршруты /panel/* монтируются только с панелью
	r.cfg.Cloudflare.Domains = append(r.cfg.Cloudflare.Domains, "manual.ddproxy.xyz")
	mux := r.buildMux()

	post := func(domain, action string) *httptest.ResponseRecorder {
		body := fmt.Sprintf(`{"domain":%q,"action":%q}`, domain, action)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/panel/api/srmd-domain", strings.NewReader(body)))
		return rec
	}

	// ручной → под СРМД
	if rec := post("manual.ddproxy.xyz", "take"); rec.Code != http.StatusOK {
		t.Fatalf("take must pass, got %d: %s", rec.Code, rec.Body.String())
	}
	// повторный take и release не-срмд-домена — ошибки
	if rec := post("manual.ddproxy.xyz", "take"); rec.Code != http.StatusBadRequest {
		t.Fatalf("double take must fail, got %d", rec.Code)
	}
	if rec := post("shared.ddproxy.xyz", "release"); rec.Code != http.StatusBadRequest {
		t.Fatalf("release of manual domain must fail, got %d", rec.Code)
	}
	// неизвестный домен
	if rec := post("nope.example.com", "take"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown domain must 404, got %d", rec.Code)
	}

	// пул сжался: 1 нода при лимите 3 → нужен 1 домен; взятый под контроль
	// ручной домен сворачивается в CNAME на основной, основной не трогается
	addHealthyNode(t, r, "node-a", "10.0.0.1")
	tickSRMD(t, r)
	if r.state.SRMD.CNames["manual.ddproxy.xyz"] != "shared.ddproxy.xyz" {
		t.Fatalf("taken domain must fold onto the base: %v", r.state.SRMD.CNames)
	}
	if _, folded := r.state.SRMD.CNames["shared.ddproxy.xyz"]; folded {
		t.Fatal("base domain must never fold")
	}

	// release свёрнутого: домен развёрнут, из created убран, отложенная
	// CNAME-запись отменена
	if rec := post("manual.ddproxy.xyz", "release"); rec.Code != http.StatusOK {
		t.Fatalf("release must pass, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(r.state.SRMD.CNames) != 0 {
		t.Fatalf("released domain must be unfolded: %v", r.state.SRMD.CNames)
	}
	if len(r.srmdPending) != 0 {
		t.Fatalf("pending CNAME write must be cancelled: %v", r.srmdPending)
	}
	for _, d := range r.state.SRMD.Created {
		if d == "manual.ddproxy.xyz" {
			t.Fatal("released domain must leave the created list")
		}
	}
	// события в журнале
	var took, released bool
	for _, ev := range r.state.Events {
		if ev.Type == EventSRMDDomainTaken && ev.Domain == "manual.ddproxy.xyz" {
			took = true
		}
		if ev.Type == EventSRMDDomainReleased && ev.Domain == "manual.ddproxy.xyz" {
			released = true
		}
	}
	if !took || !released {
		t.Fatalf("take/release events missing: took=%v released=%v", took, released)
	}
}

// ── Дефект 2: цепочки CNAME ────────────────────────────────────────────

// srmdResolveSurvivor обязан приводить к КОНЕЧНОМУ несвёрнутому домену и
// ловить цикл/self-reference. Таблица по нескольким порядкам сворачивания.
func TestSRMDResolveSurvivorNoChainNoCycle(t *testing.T) {
	cases := []struct {
		name   string
		cnames map[string]string
		start  string
		want   string
		ok     bool
	}{
		{"terminal", map[string]string{}, "s", "s", true},
		{"one hop", map[string]string{"s3": "s2"}, "s3", "s2", true},
		{"two hops", map[string]string{"s3": "s2", "s2": "s1"}, "s3", "s1", true},
		{"three hops", map[string]string{"a": "b", "b": "c", "c": "d"}, "a", "d", true},
		{"middle of chain", map[string]string{"s3": "s2", "s2": "s1"}, "s2", "s1", true},
		{"self", map[string]string{"a": "a"}, "a", "", false},
		{"two-cycle", map[string]string{"a": "b", "b": "a"}, "a", "", false},
		{"three-cycle", map[string]string{"a": "b", "b": "c", "c": "a"}, "a", "", false},
	}
	for _, c := range cases {
		got, ok := srmdResolveSurvivor(c.start, c.cnames)
		if got != c.want || ok != c.ok {
			t.Fatalf("%s: resolve(%s)=%q,%v want %q,%v", c.name, c.start, got, ok, c.want, c.ok)
		}
	}
}

// Два сворачивания подряд давали на проде цепочку shared3 -> shared2 ->
// shared1: shared2 сначала выжил (цель для shared3), потом свернулся сам.
// После fold-логики + ремонта CNAME цепочки быть не должно.
func TestSRMDFoldNeverCreatesCNAMEChain(t *testing.T) {
	r := srmdTestRegistry(t)
	r.state.SRMD.Created = []string{"shared1.ddproxy.xyz", "shared2.ddproxy.xyz", "shared3.ddproxy.xyz"}
	r.state.SRMD.CNames = map[string]string{}
	// первый шаг: активны shared, shared1, shared2, shared3; выжившие —
	// первые три, цель shared3 — наименее загруженный shared2 (клиентов 0).
	r.state.SRMD.DomainClients = map[string]int{
		"shared.ddproxy.xyz":  100,
		"shared1.ddproxy.xyz": 100,
		"shared2.ddproxy.xyz": 0,
		"shared3.ddproxy.xyz": 0,
	}
	now := time.Now()
	if !r.srmdFoldLocked(now, []string{
		"shared.ddproxy.xyz", "shared1.ddproxy.xyz",
		"shared2.ddproxy.xyz", "shared3.ddproxy.xyz",
	}, 3) {
		t.Fatal("first fold must happen")
	}
	if r.state.SRMD.CNames["shared3.ddproxy.xyz"] != "shared2.ddproxy.xyz" {
		t.Fatalf("first fold must target shared2: %v", r.state.SRMD.CNames)
	}
	// второй шаг: активны shared, shared1, shared2; теперь сворачивается
	// shared2 — раньше это и рождало shared3 -> shared2 -> shared1
	if !r.srmdFoldLocked(now, []string{
		"shared.ddproxy.xyz", "shared1.ddproxy.xyz", "shared2.ddproxy.xyz",
	}, 2) {
		t.Fatal("second fold must happen")
	}
	// ремонт цепочек: как в srmdRebalanceLocked
	r.srmdNormalizeCNameChainsLocked()

	assertNoCNAMEChain(t, r)
	if got := r.state.SRMD.CNames["shared3.ddproxy.xyz"]; got != "shared.ddproxy.xyz" {
		t.Fatalf("folded-then-repaired shared3 must point at survivor: %v", r.state.SRMD.CNames)
	}
}

// Унаследованная с прода цепочка (shared3 -> shared2 -> shared1) чинится на
// ближайшем тике селекции без ручного вмешательства.
func TestSRMDRepairsLegacyCNAMEChain(t *testing.T) {
	r := srmdTestRegistry(t)
	r.cfg.Cloudflare.Domains = []string{
		"shared.ddproxy.xyz", "shared1.ddproxy.xyz",
		"shared2.ddproxy.xyz", "shared3.ddproxy.xyz",
	}
	r.cfg.SRMD.MaxNodesPerDomain = 1 // ёмкость 1: required == queue
	r.state.SRMD.Created = []string{"shared1.ddproxy.xyz", "shared2.ddproxy.xyz", "shared3.ddproxy.xyz"}
	r.state.SRMD.CNames = map[string]string{
		"shared3.ddproxy.xyz": "shared2.ddproxy.xyz",
		"shared2.ddproxy.xyz": "shared1.ddproxy.xyz",
	}
	// 2 здоровые ноды -> required=2, активных после цепочки 2 (shared,shared1):
	// fold не срабатывает, тик занят только нормализацией цепочек
	addHealthyNode(t, r, "node-a", "10.0.0.1")
	addHealthyNode(t, r, "node-b", "10.0.0.2")

	r.evaluateAssignments(time.Now())

	if r.state.SRMD.CNames["shared3.ddproxy.xyz"] != "shared1.ddproxy.xyz" ||
		r.state.SRMD.CNames["shared2.ddproxy.xyz"] != "shared1.ddproxy.xyz" {
		t.Fatalf("legacy chain must be flattened to the survivor: %v", r.state.SRMD.CNames)
	}
	assertNoCNAMEChain(t, r)
}

// assertNoCNAMEChain — инвариант: ни одна цель CNAME не является свёрнутым
// доменом (нет цепочек) и ни один домен не свёрнут сам на себя.
func assertNoCNAMEChain(t *testing.T, r *Registry) {
	t.Helper()
	for d, target := range r.state.SRMD.CNames {
		if target == "" {
			t.Fatalf("CNAME %s has empty target", d)
		}
		if target == d {
			t.Fatalf("CNAME %s points at itself", d)
		}
		if _, folded := r.state.SRMD.CNames[target]; folded {
			t.Fatalf("CNAME chain: %s -> %s, but %s is itself folded (%v)", d, target, target, r.state.SRMD.CNames)
		}
	}
}

// ── анти-флап: только окно устойчивости (srmdStableTicks подряд) ────────

// Устойчивое условие в течение srmdStableTicks подряд тиков — действие
// обязано случиться. Кулдауна нет: как только окно набрано, СРМД действует.
func TestSRMDStableWindowSatisfiedTriggersAction(t *testing.T) {
	r := srmdTestRegistry(t)
	r.cfg.SRMD.MaxNodesPerDomain = 2 // ёмкость 2 (M=1): queue 2 -> req 1, queue 3 -> req 2
	r.cfg.Cloudflare.Domains = []string{"shared.ddproxy.xyz", "shared1.ddproxy.xyz"}
	r.state.SRMD.Created = []string{"shared1.ddproxy.xyz"}
	addHealthyNode(t, r, "node-a", "10.0.0.1")
	addHealthyNode(t, r, "node-b", "10.0.0.2")
	// queue 2 (третий узел снят) -> required 1, активных 2 -> сворачивание
	for i := 0; i < srmdStableTicks+1; i++ {
		r.evaluateAssignments(time.Now())
	}
	if r.state.Counters.SRMDFolded != 1 {
		t.Fatalf("stable fold condition must act once the window is satisfied, folded=%d",
			r.state.Counters.SRMDFolded)
	}
	if r.state.SRMD.CNames["shared1.ddproxy.xyz"] == "" {
		t.Fatalf("folded domain must have a CNAME target: %v", r.state.SRMD.CNames)
	}
	assertNoCNAMEChain(t, r)
}

// Колебание КОРОЧЕ окна устойчивости не должно вызывать ни одного действия:
// счётчик подряд идущих тиков обнуляется при смене условия, поэтому окно
// никогда не набирается. Это и есть вся защита от флапа — кулдауна нет.
func TestSRMDOscillationShorterThanWindowDoesNotAct(t *testing.T) {
	r := srmdTestRegistry(t)
	r.cfg.SRMD.MaxNodesPerDomain = 2
	r.cfg.Cloudflare.Domains = []string{"shared.ddproxy.xyz", "shared1.ddproxy.xyz"}
	r.state.SRMD.Created = []string{"shared1.ddproxy.xyz"}
	addHealthyNode(t, r, "node-a", "10.0.0.1")
	addHealthyNode(t, r, "node-b", "10.0.0.2")
	// третий узел: здоров -> queue 3 (required 2, default), снят -> queue 2
	// (required 1, fold-ветка). Каждая полоса короче окна.
	third := addHealthyNode(t, r, "node-c", "10.0.0.3")

	oscillate := func(healthy bool, ticks int) {
		third.Healthy = healthy
		for i := 0; i < ticks; i++ {
			r.evaluateAssignments(time.Now())
		}
	}
	for round := 0; round < 10; round++ {
		oscillate(false, 5) // меньше srmdStableTicks — окно не набирается
		oscillate(true, 1)  // смена условия обнуляет счётчик
	}

	if n := r.state.Counters.SRMDFolded + r.state.Counters.SRMDUnfolded; n != 0 {
		t.Fatalf("oscillation shorter than the stable window must not act, got %d transitions", n)
	}
	assertNoCNAMEChain(t, r)
}
