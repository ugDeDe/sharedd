package server

// Тесты cloud-ресурсов кабинета (stage-05): create/replay/guard, delete
// (confirm-заголовок), change-ip, management-политика, owner-изоляция,
// отсутствие секретов в ответах. Провайдер подменён моком-счётчиком
// (r.cloudFactory), поллинг-интервал сжат до миллисекунд.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"sharedd/registry/internal/control"
	"sharedd/registry/internal/nodehost"
	"sharedd/registry/internal/sshx"
)

// fastCloudPoll — сжатый поллинг-интервал на время теста.
func fastCloudPoll(t *testing.T) {
	t.Helper()
	prev := cloudPollInterval
	cloudPollInterval = time.Millisecond
	t.Cleanup(func() { cloudPollInterval = prev })
}

// fakeCloud — мок nodehost-провайдера: счётчики внешних вызовов, управляемые
// ошибки и результаты (как fakeHosting, но для cloudProvider-поверхности).
type fakeCloud struct {
	mu           sync.Mutex
	creates      int
	operations   int
	servers      int
	deletes      int
	changeIPs    int
	opStatus     string // статус операции, отдаваемый Operation()
	serverID     string
	serverIP     string
	serverStatus string
	password     string
	changeIPTo   string
	serverErr    error // не nil → Server() отвечает ошибкой
	opErr        error // не nil → Operation() отвечает ошибкой
	opFailure    *nodehost.OperationError
	changeIPErr  error
	createPanic  bool              // тест H6: паника в платном вызове
	createErr    error             // тест H1: ошибка провайдера после dispatch
	deleteFails  int               // тест H3: сколько первых Delete вернут ошибку
	inventory    []nodehost.Server // reconcile: одна страница инвентаря
	serversErr   error             // reconcile: ошибка листинга инвентаря
}

func (f *fakeCloud) Create(_ context.Context, _ nodehost.CreateRequest, _ string) (*nodehost.CreateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates++
	if f.createPanic {
		panic("injected provider panic")
	}
	if f.createErr != nil {
		return nil, f.createErr
	}
	return &nodehost.CreateResult{OperationID: "op-1", Status: "pending"}, nil
}

func (f *fakeCloud) Operation(_ context.Context, _ string) (*nodehost.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.operations++
	if f.opErr != nil {
		return nil, f.opErr
	}
	return &nodehost.Operation{ID: "op-1", Status: f.opStatus, ResourceID: f.serverID, Error: f.opFailure}, nil
}

func (f *fakeCloud) Server(_ context.Context, id string) (*nodehost.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.servers++
	if f.serverErr != nil {
		return nil, f.serverErr
	}
	return &nodehost.Server{ID: id, Status: f.serverStatus, IP: f.serverIP, Password: f.password}, nil
}

func (f *fakeCloud) Delete(_ context.Context, _ string) (*nodehost.DeleteResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes++
	if f.deleteFails > 0 {
		f.deleteFails--
		return nil, &nodehost.Error{Code: "provider_error"}
	}
	return &nodehost.DeleteResult{}, nil
}

func (f *fakeCloud) ChangeIP(_ context.Context, _ string, _ int, _ bool) (*nodehost.ChangeIPResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.changeIPs++
	if f.changeIPErr != nil {
		return nil, f.changeIPErr
	}
	return &nodehost.ChangeIPResult{IP: f.changeIPTo}, nil
}

func (f *fakeCloud) ServersPage(_ context.Context, _ string, _ int) (*nodehost.ServerPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.serversErr != nil {
		return nil, f.serversErr
	}
	return &nodehost.ServerPage{Data: f.inventory}, nil
}

func (f *fakeCloud) deleteCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deletes
}

func (f *fakeCloud) operationCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.operations
}

func (f *fakeCloud) createCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates
}

func newCloudRegistry(t *testing.T) (*Registry, *fakeCloud) {
	t.Helper()
	r, _ := newCabinetRegistry(t)
	fastCloudPoll(t)
	fake := &fakeCloud{opStatus: "succeeded", serverID: "srv-ext-1", serverIP: "203.0.113.10", serverStatus: "ACTIVE", password: "root-pw-secret-123", changeIPTo: "203.0.113.99"}
	r.cloudFactory = func(control.HostingConnection) (cloudProvider, error) { return fake, nil }
	return r, fake
}

// cloudCall — cabinetCall + произвольные заголовки (Idempotency-Key,
// X-Confirm-Action).
func cloudCall(t *testing.T, r *Registry, method, target string, cookies map[string]string, csrf, body string, headers map[string]string) *httptestResponse {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, v := range cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	if csrf != "" {
		req.Header.Set(csrfHeaderName, csrf)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	r.buildMux().ServeHTTP(rec, req)
	return wrapRec(rec)
}

// cloudSession — пользователь + сессия + nodehost-подключение (через API).
func cloudSession(t *testing.T, r *Registry, name string) (int64, int64, map[string]string, string) {
	t.Helper()
	uid := cabinetUser(t, r, name, 10)
	sess, csrf := cabinetLogin(t, r, name, "password-123")
	cookies := cookieMap(sess, csrf)
	rec := cloudCall(t, r, http.MethodPost, "/api/me/hosting-connections", cookies, csrf,
		`{"provider":"nodehost","label":"nh","credentials":{"api_key":"key-1"}}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("hosting create: %d %s", rec.Code, rec.Body)
	}
	var c struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(rec.Body), &c); err != nil || c.ID == 0 {
		t.Fatalf("hosting create decode: %v %s", err, rec.Body)
	}
	return uid, c.ID, cookies, csrf
}

const cloudCreateBodyFmt = `{"connection_id":%d,"label":"pool-node","configuration":{"name":"pool-node","cpu":"AMD Ryzen 9 7950X","cores":2,"ram":4,"disk":40,"diskType":"SSD NVMe","os":"ubuntu-24.04","location":"DE","period":"1m"}}`

func cloudCreateReq(t *testing.T, r *Registry, cookies map[string]string, csrf string, connID int64, key string) *httptestResponse {
	t.Helper()
	return cloudCall(t, r, http.MethodPost, "/api/me/cloud-resources", cookies, csrf,
		fmt.Sprintf(cloudCreateBodyFmt, connID), map[string]string{"Idempotency-Key": key})
}

func waitForCloudState(t *testing.T, r *Registry, uid, id int64, want string) control.CloudResourceV2 {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		res, err := r.control.CloudResourceOwned(id, uid)
		if err == nil && res.State == want {
			return res
		}
		time.Sleep(2 * time.Millisecond)
	}
	res, _ := r.control.CloudResourceOwned(id, uid)
	t.Fatalf("resource %d never reached %q (now %+v)", id, want, res)
	return control.CloudResourceV2{}
}

func TestCloudCreateLifecycle(t *testing.T) {
	r, fake := newCloudRegistry(t)
	uid, connID, cookies, csrf := cloudSession(t, r, "alice")

	// Отсутствие Idempotency-Key → 400 idempotency_key_required.
	rec := cloudCall(t, r, http.MethodPost, "/api/me/cloud-resources", cookies, csrf,
		fmt.Sprintf(cloudCreateBodyFmt, connID), nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body, "idempotency_key_required") {
		t.Fatalf("no idem key: %d %s", rec.Code, rec.Body)
	}

	// Невалидная конфигурация → 400 validation_failed (строк нет).
	rec = cloudCall(t, r, http.MethodPost, "/api/me/cloud-resources", cookies, csrf,
		`{"connection_id":`+fmt.Sprint(connID)+`,"configuration":{"name":"x","cpu":"c","cores":0,"ram":4,"disk":40,"diskType":"d","os":"o","location":"DE"}}`,
		map[string]string{"Idempotency-Key": "k-bad"})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body, "validation_failed") {
		t.Fatalf("bad config: %d %s", rec.Code, rec.Body)
	}
	if n, _ := r.control.CloudResourcesByUser(uid, false); len(n) != 0 {
		t.Fatalf("bad config must not create rows: %+v", n)
	}

	// Happy path: 202 → creating → ready.
	rec = cloudCreateReq(t, r, cookies, csrf, connID, "key-create-1")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var ack createAck
	if err := json.Unmarshal([]byte(rec.Body), &ack); err != nil || ack.ResourceID == 0 || ack.OperationID == 0 {
		t.Fatalf("ack decode: %v %s", err, rec.Body)
	}

	res := waitForCloudState(t, r, uid, ack.ResourceID, control.CloudResReady)
	if res.ExternalID != "srv-ext-1" || res.IP != "203.0.113.10" {
		t.Fatalf("ready resource: %+v", res)
	}
	if res.SecretsEnc == "" {
		t.Fatal("password must be sealed into secrets_enc")
	}

	// GET detail: DTO полный, пароля нет.
	rec = cloudCall(t, r, http.MethodGet, fmt.Sprintf("/api/me/cloud-resources/%d", res.ID), cookies, "", "", nil)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body, fake.password) {
		t.Fatalf("detail: %d %s", rec.Code, rec.Body)
	}
	var dto cloudResourceJSON
	if err := json.Unmarshal([]byte(rec.Body), &dto); err != nil {
		t.Fatalf("dto decode: %v", err)
	}
	if dto.ExternalID != "srv-ext-1" || dto.IP != "203.0.113.10" || dto.Provider != "nodehost" || dto.ConnectionID != connID {
		t.Fatalf("dto: %+v", dto)
	}
	if dto.LastError != "" {
		t.Fatalf("ready resource must have empty last_error: %+v", dto)
	}
	if dto.ActiveOperationID == nil || *dto.ActiveOperationID != ack.OperationID {
		t.Fatalf("active_operation_id = %v, want %d", dto.ActiveOperationID, ack.OperationID)
	}

	// Список.
	rec = cloudCall(t, r, http.MethodGet, "/api/me/cloud-resources", cookies, "", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body, `"pool-node"`) {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}

	// Replay тем же ключом: тот же resource/op, второго вызова провайдера нет.
	rec = cloudCreateReq(t, r, cookies, csrf, connID, "key-create-1")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("replay: %d %s", rec.Code, rec.Body)
	}
	var ack2 createAck
	_ = json.Unmarshal([]byte(rec.Body), &ack2)
	if ack2.ResourceID != ack.ResourceID || ack2.OperationID != ack.OperationID {
		t.Fatalf("replay mismatch: %+v vs %+v", ack2, ack)
	}
	if n := fake.createCalls(); n != 1 {
		t.Fatalf("provider create calls after replay = %d, want 1", n)
	}
}

func TestCloudCreateConcurrentSingleCall(t *testing.T) {
	r, fake := newCloudRegistry(t)
	uid, connID, cookies, csrf := cloudSession(t, r, "carol")

	const key = "key-race-1"
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := cloudCreateReq(t, r, cookies, csrf, connID, key)
			codes[i] = rec.Code
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusAccepted {
			t.Fatalf("concurrent create %d: %d", i, c)
		}
	}
	res := waitForCloudState(t, r, uid, latestCloudResource(t, r, uid), control.CloudResReady)
	_ = res
	if n := fake.createCalls(); n != 1 {
		t.Fatalf("provider create calls under race = %d, want 1", n)
	}
	list, _ := r.control.CloudResourcesByUser(uid, false)
	if len(list) != 1 {
		t.Fatalf("orphan planned rows must be cleaned: %+v", list)
	}
}

func latestCloudResource(t *testing.T, r *Registry, uid int64) int64 {
	t.Helper()
	list, err := r.control.CloudResourcesByUser(uid, false)
	if err != nil || len(list) == 0 {
		t.Fatalf("no resources: %v %+v", err, list)
	}
	return list[0].ID
}

func TestCloudCreateBegetNotImplemented(t *testing.T) {
	r, _ := newCloudRegistry(t)
	_, _, cookies, csrf := cloudSession(t, r, "begetuser")
	rec := cloudCall(t, r, http.MethodPost, "/api/me/hosting-connections", cookies, csrf,
		`{"provider":"beget","label":"b","credentials":{"cloud_login":"l","cloud_password":"p"}}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("beget conn: %d %s", rec.Code, rec.Body)
	}
	var c struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal([]byte(rec.Body), &c)
	rec = cloudCall(t, r, http.MethodPost, "/api/me/cloud-resources", cookies, csrf,
		fmt.Sprintf(`{"connection_id":%d,"configuration":{"name":"x","cpu":"c","cores":1,"ram":1,"disk":10,"diskType":"d","os":"o","location":"RU"}}`, c.ID),
		map[string]string{"Idempotency-Key": "k-beget"})
	if rec.Code != http.StatusNotImplemented || !strings.Contains(rec.Body, "not_implemented") {
		t.Fatalf("beget create: %d %s", rec.Code, rec.Body)
	}
}

func TestCloudDeleteLifecycle(t *testing.T) {
	r, fake := newCloudRegistry(t)
	uid, connID, cookies, csrf := cloudSession(t, r, "dave")
	rec := cloudCreateReq(t, r, cookies, csrf, connID, "key-del")
	var ack createAck
	_ = json.Unmarshal([]byte(rec.Body), &ack)
	res := waitForCloudState(t, r, uid, ack.ResourceID, control.CloudResReady)

	// Без confirm-заголовка → 400 confirmation_required.
	del := cloudCall(t, r, http.MethodDelete, fmt.Sprintf("/api/me/cloud-resources/%d", res.ID), cookies, csrf, "",
		map[string]string{"Idempotency-Key": "key-del-2"})
	if del.Code != http.StatusBadRequest || !strings.Contains(del.Body, "confirmation_required") {
		t.Fatalf("no confirm: %d %s", del.Code, del.Body)
	}

	// Без Idempotency-Key → 400.
	del = cloudCall(t, r, http.MethodDelete, fmt.Sprintf("/api/me/cloud-resources/%d", res.ID), cookies, csrf, "",
		map[string]string{confirmDeleteHeader: "delete"})
	if del.Code != http.StatusBadRequest || !strings.Contains(del.Body, "idempotency_key_required") {
		t.Fatalf("no key: %d %s", del.Code, del.Body)
	}

	// Happy: confirm + key → 202 → deleted; провайдер Delete ровно 1 раз.
	del = cloudCall(t, r, http.MethodDelete, fmt.Sprintf("/api/me/cloud-resources/%d", res.ID), cookies, csrf, "",
		map[string]string{"Idempotency-Key": "key-del-2", confirmDeleteHeader: "delete"})
	if del.Code != http.StatusAccepted {
		t.Fatalf("delete: %d %s", del.Code, del.Body)
	}
	fake.mu.Lock()
	fake.serverErr = &nodehost.Error{Code: "not_found"} // Server() отвечает 404 → подтверждение
	fake.mu.Unlock()
	waitForCloudState(t, r, uid, res.ID, control.CloudResDeleted)
	fake.mu.Lock()
	n := fake.deletes
	fake.mu.Unlock()
	if n != 1 {
		t.Fatalf("provider delete calls = %d, want 1", n)
	}

	// Повтор delete по deleted-ресурсу → 409 resource_busy.
	del = cloudCall(t, r, http.MethodDelete, fmt.Sprintf("/api/me/cloud-resources/%d", res.ID), cookies, csrf, "",
		map[string]string{"Idempotency-Key": "key-del-3", confirmDeleteHeader: "delete"})
	if del.Code != http.StatusConflict || !strings.Contains(del.Body, "resource_busy") {
		t.Fatalf("delete deleted: %d %s", del.Code, del.Body)
	}
}

func TestCloudDeletePhantom(t *testing.T) {
	r, fake := newCloudRegistry(t)
	uid, connID, cookies, csrf := cloudSession(t, r, "phantom")

	del := func(id int64, key string) *httptestResponse {
		return cloudCall(t, r, http.MethodDelete, fmt.Sprintf("/api/me/cloud-resources/%d", id), cookies, csrf, "",
			map[string]string{"Idempotency-Key": key, confirmDeleteHeader: "delete"})
	}
	seed := func(state string) control.CloudResourceV2 {
		t.Helper()
		res, err := r.control.CreateCloudResource(control.CloudResourceV2{
			UserID: uid, ConnectionID: connID, Provider: control.ProviderNodehost, State: control.CloudResPlanned,
		})
		if err != nil {
			t.Fatalf("seed %s: %v", state, err)
		}
		if state != control.CloudResPlanned {
			_ = r.control.UpdateCloudResourceState(res.ID, control.CloudResPlanned, control.CloudResCreating, "", "")
			if state != control.CloudResCreating {
				_ = r.control.UpdateCloudResourceState(res.ID, control.CloudResCreating, state, "", "")
			}
		}
		return res
	}
	gone := func(id int64) bool {
		_, err := r.control.CloudResourceOwned(id, uid)
		return errors.Is(err, control.ErrNotFound)
	}

	// planned, no worker → local success, row gone, no provider call.
	planned := seed(control.CloudResPlanned)
	if rec := del(planned.ID, "ph-planned"); rec.Code != http.StatusAccepted {
		t.Fatalf("phantom planned: %d %s", rec.Code, rec.Body)
	}
	if !gone(planned.ID) {
		t.Fatal("planned row must be removed")
	}

	// error (confirmed failure, never provisioned) → local success.
	failed := seed(control.CloudResError)
	_ = r.control.SetCloudResourceError(failed.ID, "insufficient_funds")
	failedOp, _, err := r.control.CreateOperation(control.Operation{
		UserID: uid, Kind: control.OpKindCloudCreate, State: control.OpFailed,
		IdemKey: "phantom-refused", LastError: "insufficient_funds",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.control.SetCloudResourceOperation(failed.ID, failedOp.ID); err != nil {
		t.Fatal(err)
	}
	if rec := del(failed.ID, "ph-error"); rec.Code != http.StatusAccepted {
		t.Fatalf("phantom error: %d %s", rec.Code, rec.Body)
	}
	if !gone(failed.ID) {
		t.Fatal("error row must be removed")
	}

	// creating with a live worker → 409, row untouched (never drop in-flight).
	inFlight := seed(control.CloudResCreating)
	opID := int64(9_000_001)
	block := make(chan struct{})
	if !r.provisionWorkers().run(opID, time.Second, func(ctx context.Context) {
		select {
		case <-block:
		case <-ctx.Done():
		}
	}) {
		t.Fatal("worker did not start")
	}
	defer close(block)
	_ = r.control.SetCloudResourceOperation(inFlight.ID, opID)
	if rec := del(inFlight.ID, "ph-busy"); rec.Code != http.StatusConflict || !strings.Contains(rec.Body, "resource_busy") {
		t.Fatalf("phantom creating busy: %d %s", rec.Code, rec.Body)
	}
	if gone(inFlight.ID) {
		t.Fatal("in-flight row must not be dropped")
	}

	// A lost worker is NOT proof that the paid instance does not exist.
	stale := seed(control.CloudResCreating)
	_ = r.control.SetCloudResourceOperation(stale.ID, opID+1)
	if rec := del(stale.ID, "ph-stale"); rec.Code != http.StatusConflict {
		t.Fatalf("phantom stale creating: %d %s", rec.Code, rec.Body)
	}
	if gone(stale.ID) {
		t.Fatal("uncertain paid outcome must be retained for reconciliation")
	}

	fake.mu.Lock()
	deletes := fake.deletes
	fake.mu.Unlock()
	if deletes != 0 {
		t.Fatalf("phantom delete called provider %d times", deletes)
	}
}

func TestCloudChangeIP(t *testing.T) {
	r, fake := newCloudRegistry(t)
	uid, connID, cookies, csrf := cloudSession(t, r, "eve")

	// not-ready → 409 resource_busy (прямая строка в planned).
	planned, err := r.control.CreateCloudResource(control.CloudResourceV2{
		UserID: uid, ConnectionID: connID, Provider: control.ProviderNodehost, State: control.CloudResPlanned,
	})
	if err != nil {
		t.Fatalf("seed planned: %v", err)
	}
	rec := cloudCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/cloud-resources/%d/change-ip", planned.ID), cookies, csrf,
		`{"subnet_id":5}`, map[string]string{"Idempotency-Key": "k-ip-0"})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body, "resource_busy") {
		t.Fatalf("not-ready change-ip: %d %s", rec.Code, rec.Body)
	}

	// Happy: ready-ресурс → 200 с новой ip, provider ChangeIP ровно 1.
	rec = cloudCreateReq(t, r, cookies, csrf, connID, "key-ip")
	var ack createAck
	_ = json.Unmarshal([]byte(rec.Body), &ack)
	res := waitForCloudState(t, r, uid, ack.ResourceID, control.CloudResReady)

	rec = cloudCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/cloud-resources/%d/change-ip", res.ID), cookies, csrf,
		`{"subnet_id":5}`, map[string]string{"Idempotency-Key": "k-ip-1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("change-ip: %d %s", rec.Code, rec.Body)
	}
	var dto cloudResourceJSON
	if err := json.Unmarshal([]byte(rec.Body), &dto); err != nil || dto.IP != "203.0.113.99" {
		t.Fatalf("change-ip dto: %v %s", err, rec.Body)
	}
	if n := fake.changeIPs; n != 1 {
		t.Fatalf("change-ip calls = %d, want 1", n)
	}

	// beget ready-ресурс → 501.
	bconn, err := func() (control.HostingConnection, error) {
		c, err := r.control.CreateHostingConnection(control.HostingConnection{
			UserID: uid, Provider: control.ProviderBeget, Label: "b", CredentialsEnc: "x",
		})
		return c, err
	}()
	if err != nil {
		t.Fatalf("beget conn: %v", err)
	}
	bres, err := r.control.CreateCloudResource(control.CloudResourceV2{
		UserID: uid, ConnectionID: bconn.ID, Provider: control.ProviderBeget,
		ExternalID: "bvps-1", State: control.CloudResReady,
	})
	if err != nil {
		t.Fatalf("seed beget: %v", err)
	}
	rec = cloudCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/cloud-resources/%d/change-ip", bres.ID), cookies, csrf,
		`{}`, map[string]string{"Idempotency-Key": "k-ip-2"})
	if rec.Code != http.StatusNotImplemented || !strings.Contains(rec.Body, "not_implemented") {
		t.Fatalf("beget change-ip: %d %s", rec.Code, rec.Body)
	}
}

func TestCloudOwnershipIsolation(t *testing.T) {
	r, _ := newCloudRegistry(t)
	uidA, connA, sessA, csrfA := cloudSession(t, r, "alice")
	_, _, sessB, csrfB := cloudSession(t, r, "mallory")

	rec := cloudCreateReq(t, r, sessA, csrfA, connA, "key-own")
	var ack createAck
	_ = json.Unmarshal([]byte(rec.Body), &ack)
	res := waitForCloudState(t, r, uidA, ack.ResourceID, control.CloudResReady)

	for _, tc := range []struct {
		method, target string
		want           int
	}{
		{http.MethodGet, fmt.Sprintf("/api/me/cloud-resources/%d", res.ID), http.StatusNotFound},
		{http.MethodDelete, fmt.Sprintf("/api/me/cloud-resources/%d", res.ID), http.StatusNotFound},
	} {
		got := cloudCall(t, r, tc.method, tc.target, sessB, csrfB, "",
			map[string]string{"Idempotency-Key": "k-f", confirmDeleteHeader: "delete"})
		if got.Code != tc.want || !strings.Contains(got.Body, "not_found") {
			t.Fatalf("%s foreign: %d %s", tc.method, got.Code, got.Body)
		}
	}
	// Чужой список пуст.
	got := cloudCall(t, r, http.MethodGet, "/api/me/cloud-resources", sessB, "", "", nil)
	if !strings.Contains(got.Body, `"resources":[]`) && !strings.Contains(got.Body, `"resources": null`) {
		t.Fatalf("foreign list: %d %s", got.Code, got.Body)
	}
}

func TestNodeManagementPolicy(t *testing.T) {
	r, _ := newCloudRegistry(t)
	uid := cabinetUser(t, r, "alice-mg", 10)
	sess, csrf := cabinetLogin(t, r, "alice-mg", "password-123")
	cookies := cookieMap(sess, csrf)
	cabinetUser(t, r, "bob-mg", 10)
	sessB, csrfB := cabinetLogin(t, r, "bob-mg", "password-123")
	cookiesB := cookieMap(sessB, csrfB)

	au, err := r.control.User(uid)
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	if err := r.control.AuthorizeNode("pool-mg1", au.User); err != nil {
		t.Fatalf("authorize: %v", err)
	}

	// GET без строки → дефолт manual/revision 1, строка не создаётся.
	rec := cloudCall(t, r, http.MethodGet, "/api/me/nodes/pool-mg1/management", cookies, "", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body, `"mode":"manual"`) || !strings.Contains(rec.Body, `"revision":1`) {
		t.Fatalf("default get: %d %s", rec.Code, rec.Body)
	}
	if _, err := r.control.NodeManagement("pool-mg1", uid); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("GET must not create the row: %v", err)
	}

	// Чужой/неизвестный → 404.
	rec = cloudCall(t, r, http.MethodGet, "/api/me/nodes/pool-mg1/management", cookiesB, "", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign get: %d %s", rec.Code, rec.Body)
	}
	rec = cloudCall(t, r, http.MethodGet, "/api/me/nodes/nope-xyz9/management", cookies, "", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown node: %d %s", rec.Code, rec.Body)
	}

	// PUT profile → 422 profile_incompatible.
	rec = cloudCall(t, r, http.MethodPut, "/api/me/nodes/pool-mg1/management", cookies, csrf,
		`{"mode":"profile","profile_version_id":24,"expected_revision":1}`, nil)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body, "profile_incompatible") {
		t.Fatalf("profile: %d %s", rec.Code, rec.Body)
	}

	// PUT manual: CAS 1 → revision 2; конфликт по устаревшей 1 → 409.
	rec = cloudCall(t, r, http.MethodPut, "/api/me/nodes/pool-mg1/management", cookies, csrf,
		`{"mode":"manual","expected_revision":1}`, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body, `"revision":2`) {
		t.Fatalf("put manual: %d %s", rec.Code, rec.Body)
	}
	rec = cloudCall(t, r, http.MethodPut, "/api/me/nodes/pool-mg1/management", cookies, csrf,
		`{"mode":"manual","expected_revision":1}`, nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body, "revision_conflict") {
		t.Fatalf("conflict: %d %s", rec.Code, rec.Body)
	}
	// Чужой PUT → 404.
	rec = cloudCall(t, r, http.MethodPut, "/api/me/nodes/pool-mg1/management", cookiesB, csrfB,
		`{"mode":"manual","expected_revision":2}`, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign put: %d %s", rec.Code, rec.Body)
	}
}

// TestNodeManagementProfileAutoLink — включение profile на свежей ноде без
// blueprint-прогона: привязанный ready-ресурс автолинкуется в
// current_resource_id (200); без ресурса и без connection_id → 422 как раньше.
func TestNodeManagementProfileAutoLink(t *testing.T) {
	r, _ := newCloudRegistry(t)
	uid, connID, cookies, csrf := cloudSession(t, r, "autolink-a")
	v := mkGraphVersion(t, r, uid, bpTestGraph())
	au, err := r.control.User(uid)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.control.AuthorizeNode("pool-al1", au.User); err != nil {
		t.Fatal(err)
	}
	if err := r.control.AuthorizeNode("pool-al2", au.User); err != nil {
		t.Fatal(err)
	}
	conn, _ := r.control.HostingConnectionOwned(connID, uid)
	res, err := r.control.CreateCloudResource(control.CloudResourceV2{
		UserID: uid, ConnectionID: conn.ID, Provider: conn.Provider, Label: "pool-al1",
		ExternalID: "srv-al1", NodeID: "pool-al1", State: control.CloudResReady,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Нода с привязанным ready-ресурсом: автолинк, 200, policy проставлен.
	rec := cloudCall(t, r, http.MethodPut, "/api/me/nodes/pool-al1/management", cookies, csrf,
		fmt.Sprintf(`{"mode":"profile","profile_version_id":%d,"expected_revision":1}`, v.ID), nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body, fmt.Sprintf(`"current_resource_id":%d`, res.ID)) {
		t.Fatalf("autolink put: %d %s", rec.Code, rec.Body)
	}
	if m, err := r.control.NodeManagement("pool-al1", uid); err != nil || m.CurrentResourceID != res.ID {
		t.Fatalf("policy current: %+v %v", m, err)
	}

	// Нода без ресурса и без connection_id: 422 как раньше.
	rec = cloudCall(t, r, http.MethodPut, "/api/me/nodes/pool-al2/management", cookies, csrf,
		fmt.Sprintf(`{"mode":"profile","profile_version_id":%d,"expected_revision":1}`, v.ID), nil)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body, "profile_incompatible") {
		t.Fatalf("no-resource put: %d %s", rec.Code, rec.Body)
	}
}

// TestCloudWorkerGuardBusyFailsOp — H1: если ключ общего guard уже занят, запуск
// cloud-воркера НЕ сбрасывается молча: операция терминально failed
// "worker_busy", ресурс уходит в терминальный error (не вечный planned).
func TestCloudWorkerGuardBusyFailsOp(t *testing.T) {
	r, fake := newCloudRegistry(t)
	uid, connID, cookies, csrf := cloudSession(t, r, "guard-a")

	// Ключ = 1: в свежей БД первый resource id и первый op id совпадают.
	// (Старое keying по resID именно тут и теряло launcher.)
	release := make(chan struct{})
	if !r.provisionWorkers().run(1, time.Hour, func(context.Context) { <-release }) {
		t.Fatal("seed guard occupy failed")
	}
	t.Cleanup(func() { close(release) })

	rec := cloudCreateReq(t, r, cookies, csrf, connID, "key-guard")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var ack createAck
	if err := json.Unmarshal([]byte(rec.Body), &ack); err != nil {
		t.Fatalf("ack: %v %s", err, rec.Body)
	}
	if ack.ResourceID != 1 || ack.OperationID != 1 {
		t.Fatalf("premise res==op==1 broken: %+v", ack)
	}
	op, err := r.control.Operation(ack.OperationID)
	if err != nil || op.State != control.OpFailed || op.LastError != "worker_busy" {
		t.Fatalf("op must be failed/worker_busy: %+v err=%v", op, err)
	}
	res, err := r.control.CloudResourceOwned(ack.ResourceID, uid)
	if err != nil || res.State != control.CloudResError {
		t.Fatalf("resource must reach terminal error: %+v err=%v", res, err)
	}
	if n := fake.createCalls(); n != 0 {
		t.Fatalf("provider must not be called, got %d", n)
	}
}

// TestCloudWorkerGuardKeyedByOpID — H1: guard key = operation id, НЕ resource id.
// Осаживаем ровно resource id (1), но op id отличается (2, поднят seed-строкой):
// create обязан пройти.
func TestCloudWorkerGuardKeyedByOpID(t *testing.T) {
	r, fake := newCloudRegistry(t)
	uid, connID, cookies, csrf := cloudSession(t, r, "guard-b")

	if _, _, err := r.control.CreateOperation(control.Operation{
		UserID: uid, Kind: control.OpKindCloudCreate, State: control.OpPending, IdemKey: "seed-bump",
	}); err != nil {
		t.Fatalf("seed op: %v", err)
	}
	// resource id будет 1, op id — 2. Осаживаем 1.
	release := make(chan struct{})
	if !r.provisionWorkers().run(1, time.Hour, func(context.Context) { <-release }) {
		t.Fatal("seed guard occupy failed")
	}
	t.Cleanup(func() { close(release) })

	rec := cloudCreateReq(t, r, cookies, csrf, connID, "key-opid")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var ack createAck
	_ = json.Unmarshal([]byte(rec.Body), &ack)
	if ack.ResourceID != 1 || ack.OperationID != 2 {
		t.Fatalf("expected res=1 op=2, got %+v", ack)
	}
	waitForCloudState(t, r, uid, ack.ResourceID, control.CloudResReady)
	if n := fake.createCalls(); n != 1 {
		t.Fatalf("create must proceed with op-id guard, provider calls=%d", n)
	}
}

// TestCloudChangeIPIdempotentReplay — H2: повтор тем же Idempotency-Key не
// вызывает провайдера второй раз и даёт идентичный ответ.
func TestCloudChangeIPIdempotentReplay(t *testing.T) {
	r, fake := newCloudRegistry(t)
	uid, connID, cookies, csrf := cloudSession(t, r, "ip-idem")
	rec := cloudCreateReq(t, r, cookies, csrf, connID, "key-ip-idem")
	var ack createAck
	_ = json.Unmarshal([]byte(rec.Body), &ack)
	res := waitForCloudState(t, r, uid, ack.ResourceID, control.CloudResReady)

	target := fmt.Sprintf("/api/me/cloud-resources/%d/change-ip", res.ID)
	first := cloudCall(t, r, http.MethodPost, target, cookies, csrf, `{"subnet_id":5}`,
		map[string]string{"Idempotency-Key": "k-ip-replay"})
	second := cloudCall(t, r, http.MethodPost, target, cookies, csrf, `{"subnet_id":5}`,
		map[string]string{"Idempotency-Key": "k-ip-replay"})
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("change-ip codes: %d/%d (%s | %s)", first.Code, second.Code, first.Body, second.Body)
	}
	if first.Body != second.Body {
		t.Fatalf("replay must be byte-identical:\n%s\n%s", first.Body, second.Body)
	}
	fake.mu.Lock()
	n := fake.changeIPs
	fake.mu.Unlock()
	if n != 1 {
		t.Fatalf("provider ChangeIP calls = %d, want 1", n)
	}
}

// TestCloudIdempotencyUserScopedAndBodyCompared — M1: ключ namespaced по
// пользователю; другое тело тем же ключом → 409 idempotency_conflict.
func TestCloudIdempotencyUserScopedAndBodyCompared(t *testing.T) {
	r, _ := newCloudRegistry(t)
	uidA, connA, sessA, csrfA := cloudSession(t, r, "ns-a")
	uidB, connB, sessB, csrfB := cloudSession(t, r, "ns-b")

	recA := cloudCreateReq(t, r, sessA, csrfA, connA, "shared-key")
	if recA.Code != http.StatusAccepted {
		t.Fatalf("A create: %d %s", recA.Code, recA.Body)
	}
	var ackA createAck
	_ = json.Unmarshal([]byte(recA.Body), &ackA)
	waitForCloudState(t, r, uidA, ackA.ResourceID, control.CloudResReady)

	recB := cloudCreateReq(t, r, sessB, csrfB, connB, "shared-key")
	if recB.Code != http.StatusAccepted {
		t.Fatalf("B create: %d %s", recB.Code, recB.Body)
	}
	var ackB createAck
	_ = json.Unmarshal([]byte(recB.Body), &ackB)
	if ackB.ResourceID == 0 || ackB.ResourceID == ackA.ResourceID {
		t.Fatalf("user B must get own resource, got %d (A=%d)", ackB.ResourceID, ackA.ResourceID)
	}
	// Чужой ресурс B не подтверждается.
	if _, err := r.control.CloudResourceOwned(ackA.ResourceID, uidB); err == nil {
		t.Fatal("user B must not own A's resource")
	}

	// Тот же пользователь A, тот же ключ, другое тело → 409.
	body2 := strings.Replace(fmt.Sprintf(cloudCreateBodyFmt, connA),
		`"label":"pool-node"`, `"label":"other-label"`, 1)
	dup := cloudCall(t, r, http.MethodPost, "/api/me/cloud-resources", sessA, csrfA, body2,
		map[string]string{"Idempotency-Key": "shared-key"})
	if dup.Code != http.StatusConflict || !strings.Contains(dup.Body, "idempotency_conflict") {
		t.Fatalf("different body same key: %d %s", dup.Code, dup.Body)
	}
}

// TestCloudSecretsNeverLeak — секрет не встречается ни в одном ответе полного
// жизненного цикла (create → detail → list → change-ip).
func TestCloudSecretsNeverLeak(t *testing.T) {
	r, fake := newCloudRegistry(t)
	uid, connID, cookies, csrf := cloudSession(t, r, "alice")
	var bodies []string
	call := func(method, target, body string, headers map[string]string) *httptestResponse {
		rec := cloudCall(t, r, method, target, cookies, csrf, body, headers)
		bodies = append(bodies, rec.Body)
		return rec
	}

	rec := call(http.MethodPost, "/api/me/cloud-resources", fmt.Sprintf(cloudCreateBodyFmt, connID), map[string]string{"Idempotency-Key": "k-leak"})
	var ack createAck
	_ = json.Unmarshal([]byte(rec.Body), &ack)
	waitForCloudState(t, r, uid, ack.ResourceID, control.CloudResReady)
	call(http.MethodGet, fmt.Sprintf("/api/me/cloud-resources/%d", ack.ResourceID), "", nil)
	call(http.MethodGet, "/api/me/cloud-resources", "", nil)
	call(http.MethodPost, fmt.Sprintf("/api/me/cloud-resources/%d/change-ip", ack.ResourceID), `{"subnet_id":5}`, map[string]string{"Idempotency-Key": "k-leak-ip"})

	for i, b := range bodies {
		if strings.Contains(b, fake.password) {
			t.Fatalf("response %d leaks the provider password: %s", i, b)
		}
	}
}

// shortCreateBudget — сжатый create-бюджет: сервер без IP обязан упереться в
// таймаут быстро.
func shortCreateBudget(t *testing.T) {
	t.Helper()
	prev := cloudCreateBudget
	cloudCreateBudget = 40 * time.Millisecond
	t.Cleanup(func() { cloudCreateBudget = prev })
}

// failExec — исполнитель, у которого SSH недоступен (check-путь).
type failExec struct{}

func (failExec) WaitSSH(context.Context, sshx.Target) error {
	return errors.New("connection refused")
}
func (failExec) Run(context.Context, sshx.Target, string, string, time.Duration) (sshx.Result, error) {
	return sshx.Result{}, errors.New("connection refused")
}

func TestCloudCreateFailureStoresSafeError(t *testing.T) {
	r, fake := newCloudRegistry(t)
	uid, connID, cookies, csrf := cloudSession(t, r, "alice")
	fake.createErr = &nodehost.Error{Code: "insufficient_funds"}

	rec := cloudCreateReq(t, r, cookies, csrf, connID, "k-fail")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var ack createAck
	_ = json.Unmarshal([]byte(rec.Body), &ack)
	res := waitForCloudState(t, r, uid, ack.ResourceID, control.CloudResError)
	if res.LastError != "insufficient_funds" {
		t.Fatalf("last_error = %q, want insufficient_funds", res.LastError)
	}
	op, err := r.control.Operation(ack.OperationID)
	if err != nil || op.State != control.OpFailed || op.LastError != "insufficient_funds" {
		t.Fatalf("operation: %+v err=%v", op, err)
	}

	rec = cloudCall(t, r, http.MethodGet, fmt.Sprintf("/api/me/cloud-resources/%d", res.ID), cookies, "", "", nil)
	var dto cloudResourceJSON
	if err := json.Unmarshal([]byte(rec.Body), &dto); err != nil {
		t.Fatalf("dto decode: %v %s", err, rec.Body)
	}
	if dto.LastError != "insufficient_funds" || dto.ActiveOperationID == nil || *dto.ActiveOperationID != ack.OperationID {
		t.Fatalf("dto truthfulness: %+v", dto)
	}
}

func TestCloudCreateRequiresIP(t *testing.T) {
	r, fake := newCloudRegistry(t)
	shortCreateBudget(t)
	uid := cabinetUser(t, r, "alice", 10)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	cookies := cookieMap(sess, csrf)
	rec := cloudCall(t, r, http.MethodPost, "/api/me/hosting-connections", cookies, csrf,
		`{"provider":"nodehost","label":"nh","credentials":{"api_key":"key-1"}}`, nil)
	var c struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal([]byte(rec.Body), &c)
	fake.serverIP = ""

	rec = cloudCreateReq(t, r, cookies, csrf, c.ID, "k-noip")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var ack createAck
	_ = json.Unmarshal([]byte(rec.Body), &ack)
	res := waitForCloudState(t, r, uid, ack.ResourceID, control.CloudResError)
	if res.LastError != "no_ip" || res.State == control.CloudResReady {
		t.Fatalf("empty-IP create must fail no_ip, got %+v", res)
	}
	if res.IP != "" {
		t.Fatalf("failed create must not publish ip: %+v", res)
	}
	// cloudWaitServer persists the billable instance handle BEFORE the IP
	// poll, so a no_ip failure keeps external_id — that handle is what a
	// later /reconcile uses to prove the server exists (found → ready).
	if res.ExternalID != "srv-ext-1" {
		t.Fatalf("failed create must keep the billable handle: %+v", res)
	}
}

func TestCloudCheck(t *testing.T) {
	r, _ := newCloudRegistry(t)
	uid, connID, cookies, csrf := cloudSession(t, r, "alice")
	res := cloudReadyResource(t, r, uid, connID, cookies, csrf, "k-check")

	// happy: fake executor отвечает успехом.
	rec := cloudCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/cloud-resources/%d/check", res.ID), cookies, csrf, `{}`, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body, `"ssh_ok":true`) {
		t.Fatalf("check happy: %d %s", rec.Code, rec.Body)
	}

	// ssh failure → 200 ssh_ok false (состояние ресурса не меняется).
	r.sshExec = failExec{}
	rec = cloudCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/cloud-resources/%d/check", res.ID), cookies, csrf, `{}`, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body, `"ssh_ok":false`) || !strings.Contains(rec.Body, "ssh_unreachable") {
		t.Fatalf("check fail: %d %s", rec.Code, rec.Body)
	}
	if got, _ := r.control.CloudResourceOwned(res.ID, uid); got.State != control.CloudResReady {
		t.Fatalf("check must not change state, got %s", got.State)
	}

	// not_ready.
	planned, err := r.control.CreateCloudResource(control.CloudResourceV2{
		UserID: uid, ConnectionID: connID, Provider: control.ProviderNodehost, State: control.CloudResPlanned,
	})
	if err != nil {
		t.Fatalf("seed planned: %v", err)
	}
	rec = cloudCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/cloud-resources/%d/check", planned.ID), cookies, csrf, `{}`, nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body, "not_ready") {
		t.Fatalf("check not_ready: %d %s", rec.Code, rec.Body)
	}

	// ssh_unavailable: ready без secrets_enc.
	bare, err := r.control.CreateCloudResource(control.CloudResourceV2{
		UserID: uid, ConnectionID: connID, Provider: control.ProviderNodehost, State: control.CloudResPlanned,
	})
	if err != nil {
		t.Fatalf("seed bare: %v", err)
	}
	_ = r.control.UpdateCloudResourceState(bare.ID, control.CloudResPlanned, control.CloudResCreating, "", "")
	_ = r.control.UpdateCloudResourceState(bare.ID, control.CloudResCreating, control.CloudResReady, "srv-x", "203.0.113.7")
	rec = cloudCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/cloud-resources/%d/check", bare.ID), cookies, csrf, `{}`, nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body, "ssh_unavailable") {
		t.Fatalf("check ssh_unavailable: %d %s", rec.Code, rec.Body)
	}

	// beget → 501.
	bconn, err := r.control.CreateHostingConnection(control.HostingConnection{
		UserID: uid, Provider: control.ProviderBeget, Label: "b", CredentialsEnc: "x",
	})
	if err != nil {
		t.Fatalf("beget conn: %v", err)
	}
	bres, err := r.control.CreateCloudResource(control.CloudResourceV2{
		UserID: uid, ConnectionID: bconn.ID, Provider: control.ProviderBeget, State: control.CloudResReady,
	})
	if err != nil {
		t.Fatalf("beget res: %v", err)
	}
	rec = cloudCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/cloud-resources/%d/check", bres.ID), cookies, csrf, `{}`, nil)
	if rec.Code != http.StatusNotImplemented || !strings.Contains(rec.Body, "not_implemented") {
		t.Fatalf("check beget: %d %s", rec.Code, rec.Body)
	}
}

func TestCloudListVisibilityAndZombieReconcile(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	uid, connID, cookies, _ := cloudSession(t, r, "cloud-list")

	seed := func(state string) control.CloudResourceV2 {
		res, err := r.control.CreateCloudResource(control.CloudResourceV2{
			UserID: uid, ConnectionID: connID, Provider: control.ProviderNodehost, Label: "r", State: state,
		})
		if err != nil {
			t.Fatalf("seed %s: %v", state, err)
		}
		return res
	}

	// Zombie: planned + failed operation → error с кодом операции.
	zombie := seed(control.CloudResPlanned)
	op, _, err := r.control.CreateOperation(control.Operation{
		UserID: uid, Kind: control.OpKindCloudCreate, State: control.OpFailed,
		IdemKey: "zombie-1", LastError: "insufficient_funds",
	})
	if err != nil {
		t.Fatalf("zombie op: %v", err)
	}
	_ = r.control.SetCloudResourceOperation(zombie.ID, op.ID)

	// Живой creating + свежая pending-операция — не трогаем.
	live := seed(control.CloudResCreating)
	op2, _, err := r.control.CreateOperation(control.Operation{
		UserID: uid, Kind: control.OpKindCloudCreate, State: control.OpPending, IdemKey: "pending-1",
	})
	if err != nil {
		t.Fatalf("pending op: %v", err)
	}
	_ = r.control.SetCloudResourceOperation(live.ID, op2.ID)

	// deleted скрыт из списка по умолчанию, но доступен с include_deleted=1.
	deleted := seed(control.CloudResDeleted)

	rec := cloudCall(t, r, http.MethodGet, "/api/me/cloud-resources", cookies, "", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	var list struct {
		Resources []cloudResourceJSON `json:"resources"`
	}
	if err := json.Unmarshal([]byte(rec.Body), &list); err != nil {
		t.Fatalf("decode: %v %s", err, rec.Body)
	}
	byID := map[int64]cloudResourceJSON{}
	for _, x := range list.Resources {
		byID[x.ID] = x
	}
	if _, ok := byID[deleted.ID]; ok {
		t.Fatalf("deleted resource leaked into default list: %+v", list.Resources)
	}
	if got := byID[zombie.ID]; got.State != control.CloudResError || got.LastError != "insufficient_funds" {
		t.Fatalf("zombie not reconciled: %+v", got)
	}
	if got := byID[live.ID]; got.State != control.CloudResCreating || got.LastError != "" {
		t.Fatalf("live creating must stay: %+v", got)
	}

	rec = cloudCall(t, r, http.MethodGet, "/api/me/cloud-resources?include_deleted=1", cookies, "", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body, fmt.Sprintf(`"id":%d`, deleted.ID)) {
		t.Fatalf("include_deleted: %d %s", rec.Code, rec.Body)
	}

	// Detail тоже реконсилит зомби.
	rec = cloudCall(t, r, http.MethodGet, fmt.Sprintf("/api/me/cloud-resources/%d", zombie.ID), cookies, "", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body, `"state":"error"`) || !strings.Contains(rec.Body, "insufficient_funds") {
		t.Fatalf("detail zombie: %d %s", rec.Code, rec.Body)
	}
}

func TestCloudCreateFailsFastOn404Operation(t *testing.T) {
	r, fake := newCloudRegistry(t)
	fake.opErr = &nodehost.Error{Code: "not_found", HTTPStatus: http.StatusNotFound}
	uid, connID, cookies, csrf := cloudSession(t, r, "cloud-404")

	rec := cloudCreateReq(t, r, cookies, csrf, connID, "key-404")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var ack createAck
	_ = json.Unmarshal([]byte(rec.Body), &ack)

	// Без fail-fast это ждало бы cloudCreateBudget (10м) — тест ловит за 5с.
	res := waitForCloudState(t, r, uid, ack.ResourceID, control.CloudResError)
	if res.LastError != "not_found" {
		t.Fatalf("last_error = %q, want not_found", res.LastError)
	}
	if n := fake.operationCalls(); n != 1 {
		t.Fatalf("operation polls = %d, want 1 (no budget wait)", n)
	}
}
