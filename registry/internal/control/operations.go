package control

// Durable-журнал платных операций провайдера и облачных ресурсов
// (docs/specs/beget-provisioning.md §1, §4). Деньги-критичные инварианты:
//   - строка operations(state=pending) персистится ДО вызова платного API;
//   - UNIQUE(idem_key) гарантирует ровно один заказ на ключ (в т.ч. под гонкой);
//   - state=unknown разрешается только сверкой (GetList), не повторным POST;
//   - ошибка удаления НЕ очищает provider_vps_id.

import (
	"database/sql"
	"errors"
	"time"
)

// Состояния операции (beget-provisioning.md §1). unknown — ответ потерян/неоднозначен.
const (
	OpPending   = "pending"
	OpSent      = "sent"
	OpConfirmed = "confirmed"
	OpUnknown   = "unknown"
	OpFailed    = "failed"
	OpDeleting  = "deleting"
	OpDeleted   = "deleted"
	// Установка профиля на созданный VPS (addendum-profiles-vps.md §4.3):
	// installing → done|failed. Не участвуют в денежной логике (место держит
	// confirmed/cloud_resources).
	OpInstalling = "installing"
	OpDone       = "done"
	// waiting_vps — платный заказ подтверждён, но VPS не дошёл до RUNNING в
	// отведённое окно (addendum §7.3): место в квоте ЗАНЯТО, новый заказ
	// невозможен, установка не запущена. Разрешается повторной проверкой/
	// retry-install, не новым CreateVps.
	OpWaitingVps = "waiting_vps"
)

// OpKindCreate/OpKindRemove — виды платных операций.
const (
	OpKindCreate = "create"
	OpKindRemove = "remove"
	// OpKindManualInstall — установка на СВОЙ сервер (без провайдера):
	// деньги-инварианты неприменимы, место держит AuthorizeNode, а не заказ.
	OpKindManualInstall = "manual_install"
)

// Operation — строка журнала. RequestJSON намеренно НЕ сериализуется в API
// (json:"-"): в теле могут оказаться чувствительные параметры.
type Operation struct {
	ID            int64  `json:"id"`
	UserID        int64  `json:"user_id"`
	NodeID        string `json:"node_id"`
	Kind          string `json:"kind"`
	State         string `json:"state"`
	IdemKey       string `json:"idem_key"`
	ProviderVPSID string `json:"provider_vps_id,omitempty"`
	Region        string `json:"region,omitempty"`
	ConfigID      string `json:"config_id,omitempty"`
	// ProfileID — профиль установки, выбранный оператором при создании.
	// retry-install использует его, чтобы не подменить профиль; 0 = не задан.
	ProfileID   int64  `json:"profile_id"`
	RequestJSON string `json:"-"`
	LastError   string `json:"last_error,omitempty"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

const opCols = `id, user_id, node_id, kind, state, idem_key, provider_vps_id, region, config_id, profile_id, request_json, last_error, created_at, updated_at`

func scanOp(row interface{ Scan(...any) error }) (Operation, error) {
	var o Operation
	err := row.Scan(&o.ID, &o.UserID, &o.NodeID, &o.Kind, &o.State, &o.IdemKey, &o.ProviderVPSID,
		&o.Region, &o.ConfigID, &o.ProfileID, &o.RequestJSON, &o.LastError, &o.CreatedAt, &o.UpdatedAt)
	return o, err
}

// CreateOperation персистит операцию до вызова API. Если idem_key уже занят
// (в т.ч. конкурентным запросом) — возвращает существующую строку и created=false.
// Это атомарный «шлагбаум»: второй запрос того же ключа CreateVps не вызовет.
func (s *Store) CreateOperation(op Operation) (Operation, bool, error) {
	now := time.Now().Unix()
	res, err := s.sql.Exec(
		`INSERT INTO operations (user_id, node_id, kind, state, idem_key, provider_vps_id, region, config_id, profile_id, request_json, last_error, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(idem_key) DO NOTHING`,
		op.UserID, op.NodeID, op.Kind, op.State, op.IdemKey, op.ProviderVPSID, op.Region, op.ConfigID, op.ProfileID, op.RequestJSON, op.LastError, now, now)
	if err != nil {
		return Operation{}, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		existing, err := s.OperationByIdemKey(op.IdemKey)
		return existing, false, err
	}
	op.ID, _ = res.LastInsertId()
	op.CreatedAt, op.UpdatedAt = now, now
	return op, true, nil
}

// CreateLimits — пределы платного создания, проверяются В ОДНОЙ транзакции
// со вставкой operations (C1/H1/H3). MaxNodes — общий бюджет мест пользователя
// (ноды ∪ подтверждённые VPS ∪ незавершённые заказы).
type CreateLimits struct {
	MaxNodes         int
	MaxCreatesPerDay int
	CooldownErrors   int
	Cooldown         time.Duration
	Now              time.Time
}

// ReserveCreateOperation — атомарный шлагбаум платного заказа. В одной
// транзакции: (1) INSERT operations(pending) ON CONFLICT(idem_key) DO NOTHING;
// при вставке поверх неё же проверяются (2) отсутствие другой незавершённой
// create-операции пользователя (H3), (3) квота места по node_owners ∪
// cloud_resources ∪ незавершённым create-операциям (C1/H1), (4) дневной лимит,
// (5) cooldown после серии ошибок. Любое нарушение → ROLLBACK + sentinel:
// ровно одна оплата выживает под гонкой, а подтверждённый VPS занимает место.
// Ключ уже занят → создан=false и существующая строка (owner-проверка — в
// provision). Транзакция завершается ДО вызова провайдера: pending-строка и
// есть резерв.
func (s *Store) ReserveCreateOperation(op Operation, lim CreateLimits) (Operation, bool, error) {
	tx, err := s.sql.Begin()
	if err != nil {
		return Operation{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	now := lim.Now
	if now.IsZero() {
		now = time.Now()
	}
	res, err := tx.Exec(
		`INSERT INTO operations (user_id, node_id, kind, state, idem_key, provider_vps_id, region, config_id, profile_id, request_json, last_error, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(idem_key) DO NOTHING`,
		op.UserID, op.NodeID, op.Kind, op.State, op.IdemKey, op.ProviderVPSID, op.Region, op.ConfigID, op.ProfileID, op.RequestJSON, op.LastError, now.Unix(), now.Unix())
	if err != nil {
		return Operation{}, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// ключ занят — читаем существующую строку ВНУТРИ транзакции (единственное
		// соединение пула занято tx; s.* дедлочил бы).
		existing, err := scanOp(tx.QueryRow(`SELECT `+opCols+` FROM operations WHERE idem_key = ?`, op.IdemKey))
		if errors.Is(err, sql.ErrNoRows) {
			return Operation{}, false, ErrNotFound
		}
		if err != nil {
			return Operation{}, false, err
		}
		if err := tx.Commit(); err != nil {
			return Operation{}, false, err
		}
		return existing, false, nil
	}
	op.ID, _ = res.LastInsertId()
	op.CreatedAt, op.UpdatedAt = now.Unix(), now.Unix()

	// C1/H1: место = distinct node_id по нодам, активным облачным ресурсам и
	// всем заказам, которые ещё держат ресурс (pending/sent/confirmed/unknown/
	// deleting; failed/deleted места не занимают). Включая только что
	// вставленную pending-строку. Confirmed-строка учтена даже в окне между
	// state=confirmed и записью cloud_resources.
	var used int
	if err := tx.QueryRow(
		`SELECT COUNT(DISTINCT node_id) FROM (
		   SELECT node_id FROM node_owners WHERE user_id = ? AND legacy = 0 AND node_id <> ''
		   UNION SELECT node_id FROM cloud_resources WHERE user_id = ? AND state <> 'deleted' AND node_id <> ''
		   UNION SELECT node_id FROM operations WHERE user_id = ? AND kind = ? AND state NOT IN ('failed','deleted') AND node_id <> ''
		 )`, op.UserID, op.UserID, op.UserID, OpKindCreate).Scan(&used); err != nil {
		return Operation{}, false, err
	}
	if used > lim.MaxNodes {
		return Operation{}, false, ErrQuotaExceeded
	}

	// Дневной лимит: фактически начатые заказы (pending/sent/confirmed/unknown).
	if lim.MaxCreatesPerDay > 0 {
		var n int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM operations WHERE user_id = ? AND kind = ? AND created_at >= ? AND state IN ('pending','sent','confirmed','unknown')`,
			op.UserID, OpKindCreate, now.Add(-24*time.Hour).Unix()).Scan(&n); err != nil {
			return Operation{}, false, err
		}
		if n > lim.MaxCreatesPerDay {
			return Operation{}, false, ErrRateLimited
		}
	}

	// Cooldown: последние CooldownErrors create-операций (без нашей строки) —
	// все failed/unknown и самая свежая в пределах окна.
	if lim.CooldownErrors > 0 && lim.Cooldown > 0 {
		rows, err := tx.Query(
			`SELECT state, updated_at FROM operations WHERE user_id = ? AND kind = ? AND id <> ? ORDER BY id DESC LIMIT ?`,
			op.UserID, OpKindCreate, op.ID, lim.CooldownErrors)
		if err != nil {
			return Operation{}, false, err
		}
		type st struct {
			state string
			upd   int64
		}
		var recent []st
		for rows.Next() {
			var e st
			if err := rows.Scan(&e.state, &e.upd); err != nil {
				_ = rows.Close()
				return Operation{}, false, err
			}
			recent = append(recent, e)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return Operation{}, false, err
		}
		if len(recent) == lim.CooldownErrors {
			allBad := true
			for _, e := range recent {
				if e.state != OpFailed && e.state != OpUnknown {
					allBad = false
					break
				}
			}
			if allBad && now.Sub(time.Unix(recent[0].upd, 0)) < lim.Cooldown {
				return Operation{}, false, ErrCooldown
			}
		}
	}

	// H3: другой незавершённый заказ пользователя (кроме только что вставленного) —
	// повторный платный заказ запрещён до сверки/завершения. Проверяется после
	// cooldown: серия ошибок даёт приоритетный ErrCooldown.
	var other int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM operations WHERE user_id = ? AND kind = ? AND idem_key <> ? AND state IN ('pending','sent','unknown')`,
		op.UserID, OpKindCreate, op.IdemKey).Scan(&other); err != nil {
		return Operation{}, false, err
	}
	if other > 0 {
		return Operation{}, false, ErrCreateInProgress
	}

	if err := tx.Commit(); err != nil {
		return Operation{}, false, err
	}
	return op, true, nil
}

// MarkCreateSent — CAS pending→sent, вызывается непосредственно ПЕРЕД платным
// CreateVps. true — резерв наш, провайдера звать можно. false — строка уже не
// pending (например, отменена в окне до вызова): платный вызов НЕ выполняется
// (R1: отмена в полёте не должна порождать заказ).
func (s *Store) MarkCreateSent(id int64) (bool, error) {
	res, err := s.sql.Exec(
		`UPDATE operations SET state = ?, updated_at = ? WHERE id = ? AND state = ?`,
		OpSent, time.Now().Unix(), id, OpPending)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// MarkInstallingIfActive — CAS в installing для операции в нетерминальном
// состоянии (retry-install: старт фоновой установки). false = уже done/deleted
// — повтор установки не нужен, платный вызов не делаем.
func (s *Store) MarkInstallingIfActive(id int64) (bool, error) {
	res, err := s.sql.Exec(
		`UPDATE operations SET state = ?, updated_at = ? WHERE id = ? AND state NOT IN (?,?)`,
		OpInstalling, time.Now().Unix(), id, OpDone, OpDeleted)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// CancelPendingOperation — CAS pending→failed. Отмена разрешена только из
// pending; false — платный заказ уже ушёл (sent/confirmed/unknown) или
// завершён. CAS закрывает гонку с MarkCreateSent: ровно одно из двух переходов
// выигрывает, слот не освобождается на живом заказе (R1).
func (s *Store) CancelPendingOperation(id int64) (bool, error) {
	res, err := s.sql.Exec(
		`UPDATE operations SET state = ?, last_error = ?, updated_at = ? WHERE id = ? AND state = ?`,
		OpFailed, "cancelled by user", time.Now().Unix(), id, OpPending)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// UsedSlots — занятые места пользователя: distinct node_id по привязанным
// нодам, активным облачным ресурсам и заказам, держащим ресурс. Не включает
// ещё не созданную операцию — для предварительного (advisory) отказа в
// кабинете; авторитетная проверка — в ReserveCreateOperation.
func (s *Store) UsedSlots(userID int64) (int, error) {
	var n int
	err := s.sql.QueryRow(
		`SELECT COUNT(DISTINCT node_id) FROM (
		   SELECT node_id FROM node_owners WHERE user_id = ? AND legacy = 0 AND node_id <> ''
		   UNION SELECT node_id FROM cloud_resources WHERE user_id = ? AND state <> 'deleted' AND node_id <> ''
		   UNION SELECT node_id FROM operations WHERE user_id = ? AND kind = ? AND state NOT IN ('failed','deleted') AND node_id <> ''
		 )`, userID, userID, userID, OpKindCreate).Scan(&n)
	return n, err
}

func (s *Store) OperationByIdemKey(idemKey string) (Operation, error) {
	o, err := scanOp(s.sql.QueryRow(`SELECT `+opCols+` FROM operations WHERE idem_key = ?`, idemKey))
	if errors.Is(err, sql.ErrNoRows) {
		return Operation{}, ErrNotFound
	}
	return o, err
}

func (s *Store) Operation(id int64) (Operation, error) {
	o, err := scanOp(s.sql.QueryRow(`SELECT `+opCols+` FROM operations WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Operation{}, ErrNotFound
	}
	return o, err
}

// UpdateOperationState — перевод состояния. vpsID/lastErr пустые = не менять.
func (s *Store) UpdateOperationState(id int64, state, vpsID, lastErr string) error {
	_, err := s.sql.Exec(
		`UPDATE operations SET state = ?,
		    provider_vps_id = CASE WHEN ? = '' THEN provider_vps_id ELSE ? END,
		    last_error = ?, updated_at = ? WHERE id = ?`,
		state, vpsID, vpsID, lastErr, time.Now().Unix(), id)
	return err
}

// CreatesInLast24h — число фактически отправленных платных заказов за сутки.
// Считаются только состояния, означающие реальный вызов API (sent/confirmed/
// unknown); failed до вызова — не заказ.
func (s *Store) CreatesInLast24h(userID int64, since int64) (int, error) {
	var n int
	err := s.sql.QueryRow(
		`SELECT COUNT(*) FROM operations WHERE user_id = ? AND kind = ? AND created_at >= ?
		   AND state IN ('sent','confirmed','unknown')`,
		userID, OpKindCreate, since).Scan(&n)
	return n, err
}

// RecentCreateOps — последние N create-операций пользователя (новые первыми)
// для расчёта cooldown после серии ошибок.
func (s *Store) RecentCreateOps(userID int64, limit int) ([]Operation, error) {
	rows, err := s.sql.Query(`SELECT `+opCols+` FROM operations WHERE user_id = ? AND kind = ? ORDER BY id DESC LIMIT ?`,
		userID, OpKindCreate, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Operation
	for rows.Next() {
		o, err := scanOp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// OperationsByUser — журнал операций пользователя для кабинета (новые первыми).
// Включает state=unknown (ответ потерян) — пользователь должен их видеть.
func (s *Store) OperationsByUser(userID int64) ([]Operation, error) {
	rows, err := s.sql.Query(`SELECT `+opCols+` FROM operations WHERE user_id = ? ORDER BY id DESC LIMIT 200`, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Operation
	for rows.Next() {
		o, err := scanOp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// UnknownOperations — операции, ждущие сверки через GetList.
func (s *Store) UnknownOperations() ([]Operation, error) {
	rows, err := s.sql.Query(`SELECT `+opCols+` FROM operations WHERE state = ? ORDER BY id ASC`, OpUnknown)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Operation
	for rows.Next() {
		o, err := scanOp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// InstallingOperations — операции, застигнутые в state=installing. Нужны
// resume-пути при старте реестра (DEF-C): фоновые воркеры не персистятся, и
// после рестарта установка осталась бы навсегда «in progress».
func (s *Store) InstallingOperations() ([]Operation, error) {
	rows, err := s.sql.Query(`SELECT `+opCols+` FROM operations WHERE state = ? ORDER BY id ASC`, OpInstalling)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Operation
	for rows.Next() {
		o, err := scanOp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ── облачные ресурсы ──────────────────────────────────────────────

// CloudResource — учёт VPS, созданных для пользователя.
type CloudResource struct {
	VPSID     string `json:"vps_id"`
	UserID    int64  `json:"user_id"`
	NodeID    string `json:"node_id,omitempty"`
	Region    string `json:"region,omitempty"`
	ConfigID  string `json:"config_id,omitempty"`
	State     string `json:"state"`
	CreatedAt int64  `json:"created_at"`
	DeletedAt int64  `json:"deleted_at,omitempty"`
}

// UpsertCloudResource создаёт/обновляет ресурс по vps_id.
func (s *Store) UpsertCloudResource(r CloudResource) error {
	_, err := s.sql.Exec(
		`INSERT INTO cloud_resources (vps_id, user_id, node_id, region, config_id, state, created_at, deleted_at)
		 VALUES (?,?,?,?,?,?,?,?)
		 ON CONFLICT(vps_id) DO UPDATE SET node_id=excluded.node_id, region=excluded.region,
		   config_id=excluded.config_id, state=excluded.state`,
		r.VPSID, r.UserID, r.NodeID, r.Region, r.ConfigID, r.State, orNow(r.CreatedAt), r.DeletedAt)
	return err
}

// SetCloudResourceState — смена состояния ресурса (deleted проставляет время).
func (s *Store) SetCloudResourceState(vpsID, state string) error {
	_, err := s.sql.Exec(
		`UPDATE cloud_resources SET state = ?, deleted_at = CASE WHEN ? = 'deleted' THEN ? ELSE deleted_at END WHERE vps_id = ?`,
		state, state, time.Now().Unix(), vpsID)
	return err
}

func (s *Store) CloudResource(vpsID string) (CloudResource, error) {
	var r CloudResource
	err := s.sql.QueryRow(
		`SELECT vps_id, user_id, node_id, region, config_id, state, created_at, deleted_at FROM cloud_resources WHERE vps_id = ?`,
		vpsID).Scan(&r.VPSID, &r.UserID, &r.NodeID, &r.Region, &r.ConfigID, &r.State, &r.CreatedAt, &r.DeletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return CloudResource{}, ErrNotFound
	}
	return r, err
}

func orNow(ts int64) int64 {
	if ts == 0 {
		return time.Now().Unix()
	}
	return ts
}
