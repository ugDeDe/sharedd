package control

// Постоянные логические ноды пользователя (node_slots).
//
// Технический node_id может измениться при переустановке или замене VPS.
// Пользовательский номер ("Нода №2") должен остаться прежним. Слот создаётся
// при первой привязке владельца; legacy-регистрации без пользователя слотов
// не получают и в публичной статистике остаются под именем из регистрации.

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// NodeSlot — постоянное место ноды в аккаунте.
type NodeSlot struct {
	ID         int64  `json:"id"`
	UserID     int64  `json:"user_id"`
	Number     int64  `json:"number"`
	Name       string `json:"name"`
	Revision   int64  `json:"revision"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
	ArchivedAt int64  `json:"archived_at,omitempty"`
}

// PublicNodeIdentity — безопасные поля для публичной подписи ноды.
type PublicNodeIdentity struct {
	NodeID          string
	Username        string
	OwnerNodeNumber int64
}

// DisplayName — серверное правило публичной подписи. Legacy/неизвестная нода
// остаётся под техническим именем; логин не придумывается.
func (p PublicNodeIdentity) DisplayName() string {
	if p.Username == "" || p.OwnerNodeNumber <= 0 {
		return p.NodeID
	}
	return fmt.Sprintf("%s · Нода №%d", p.Username, p.OwnerNodeNumber)
}

func slotNameFromNodeID(nodeID string) string {
	name := nodeID
	if i := strings.LastIndexByte(nodeID, '-'); i > 0 {
		name = nodeID[:i]
	}
	name = strings.TrimSpace(name)
	name = strings.Trim(name, "._-")
	if len(name) > 10 {
		name = name[:10]
	}
	name = strings.Trim(name, "._-")
	if name == "" {
		return "node"
	}
	return name
}

// ensureSlotTx создаёт новый слот и возвращает его. Вызывать внутри
// транзакции, которая уже сериализует писателей (Store использует одно
// соединение): counter и INSERT видят одно состояние.
func ensureSlotTx(tx *sql.Tx, userID int64, name string) (NodeSlot, error) {
	if userID <= 0 {
		return NodeSlot{}, ErrValidation
	}
	now := time.Now().Unix()
	if _, err := tx.Exec(`INSERT INTO user_node_counters (user_id, next_number) VALUES (?, 1)
		ON CONFLICT(user_id) DO NOTHING`, userID); err != nil {
		return NodeSlot{}, err
	}
	var number int64
	if err := tx.QueryRow(`SELECT next_number FROM user_node_counters WHERE user_id = ?`, userID).Scan(&number); err != nil {
		return NodeSlot{}, err
	}
	if number <= 0 {
		number = 1
	}
	if _, err := tx.Exec(`UPDATE user_node_counters SET next_number = ? WHERE user_id = ?`, number+1, userID); err != nil {
		return NodeSlot{}, err
	}
	if strings.TrimSpace(name) == "" {
		name = "node"
	}
	res, err := tx.Exec(`INSERT INTO node_slots (user_id, number, name, revision, created_at, updated_at)
		VALUES (?,?,?,1,?,?)`, userID, number, name, now, now)
	if err != nil {
		return NodeSlot{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return NodeSlot{}, err
	}
	return NodeSlot{ID: id, UserID: userID, Number: number, Name: name, Revision: 1, CreatedAt: now, UpdatedAt: now}, nil
}

// BindSlotTx привязывает слот к конкретному техническому node_id.
func BindSlotTx(tx *sql.Tx, nodeID string, userID, slotID int64) error {
	if slotID <= 0 || userID <= 0 {
		return ErrValidation
	}
	res, err := tx.Exec(`UPDATE node_owners SET slot_id = ?
		WHERE node_id = ? AND user_id = ? AND legacy = 0`, slotID, nodeID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrOwnerMismatch
	}
	return nil
}

// slotForNodeTx — слот уже привязанного технического node_id (0 = нет).
func slotForNodeTx(tx *sql.Tx, nodeID string, userID int64) (int64, error) {
	var slot sql.NullInt64
	err := tx.QueryRow(`SELECT slot_id FROM node_owners WHERE node_id = ? AND user_id = ? AND legacy = 0`,
		nodeID, userID).Scan(&slot)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return slot.Int64, nil
}

// previousNodeForReplacementTx — технический node_id заменяемого ресурса.
// Нужен, чтобы новая нода унаследовала постоянный номер слота при замене VPS.
func previousNodeForReplacementTx(tx *sql.Tx, operationID int64) string {
	if operationID <= 0 {
		return ""
	}
	var nodeID string
	_ = tx.QueryRow(`SELECT prev.node_id
		FROM cloud_resources2 cur
		JOIN cloud_resources2 old ON old.id = cur.replaces_resource_id
		JOIN node_owners prev ON prev.node_id = old.node_id
		WHERE cur.last_operation_id = ? AND prev.legacy = 0`, operationID).Scan(&nodeID)
	return nodeID
}

// EnsureClaimSlotTx выбирает слот для claim-on-first-register:
//   - у новой ноды слота нет — создаём;
//   - при переустановке со старым operation.node_id переиспользуем слот
//     старой технической ноды, чтобы номер не изменился;
//   - иначе создаём новый.
func EnsureClaimSlotTx(tx *sql.Tx, userID int64, nodeID, previousNodeID string) (int64, error) {
	if slot, err := slotForNodeTx(tx, nodeID, userID); err != nil || slot > 0 {
		return slot, err
	}
	if previousNodeID != "" && previousNodeID != nodeID {
		if slot, err := slotForNodeTx(tx, previousNodeID, userID); err != nil {
			return 0, err
		} else if slot > 0 {
			return slot, nil
		}
	}
	slot, err := ensureSlotTx(tx, userID, slotNameFromNodeID(nodeID))
	if err != nil {
		return 0, err
	}
	return slot.ID, nil
}

// NodeSlotForNode — слот владельца технической ноды.
func (s *Store) NodeSlotForNode(nodeID string) (NodeSlot, bool, error) {
	var v NodeSlot
	var archived sql.NullInt64
	err := s.sql.QueryRow(`SELECT s.id, s.user_id, s.number, s.name, s.revision, s.created_at, s.updated_at, s.archived_at
		FROM node_slots s JOIN node_owners o ON o.slot_id = s.id
		WHERE o.node_id = ? AND o.legacy = 0`, nodeID).Scan(
		&v.ID, &v.UserID, &v.Number, &v.Name, &v.Revision, &v.CreatedAt, &v.UpdatedAt, &archived)
	if errors.Is(err, sql.ErrNoRows) {
		return NodeSlot{}, false, nil
	}
	if err != nil {
		return NodeSlot{}, false, err
	}
	v.ArchivedAt = archived.Int64
	return v, true, nil
}

// RenameNodeSlot — меняет только отображаемое/будущее имя логической ноды.
// Технический node_id, секрет, владелец и квота не затрагиваются.
func (s *Store) RenameNodeSlot(userID, slotID int64, name string, expectedRevision int64) (NodeSlot, error) {
	name = strings.TrimSpace(name)
	if userID <= 0 || slotID <= 0 || name == "" || len(name) > 10 || expectedRevision <= 0 {
		return NodeSlot{}, ErrValidation
	}
	res, err := s.sql.Exec(`UPDATE node_slots SET name = ?, revision = revision + 1, updated_at = ?
		WHERE id = ? AND user_id = ? AND archived_at IS NULL AND revision = ?`,
		name, time.Now().Unix(), slotID, userID, expectedRevision)
	if err != nil {
		return NodeSlot{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Различаем чужой/несуществующий слот и конфликт ревизии.
		var rev int64
		err := s.sql.QueryRow(`SELECT revision FROM node_slots WHERE id = ? AND user_id = ?`,
			slotID, userID).Scan(&rev)
		if errors.Is(err, sql.ErrNoRows) {
			return NodeSlot{}, ErrNotFound
		}
		if err != nil {
			return NodeSlot{}, err
		}
		return NodeSlot{}, ErrRevisionConflict
	}
	return s.nodeSlotByID(slotID, userID)
}

func (s *Store) nodeSlotByID(id, userID int64) (NodeSlot, error) {
	var v NodeSlot
	var archived sql.NullInt64
	err := s.sql.QueryRow(`SELECT id, user_id, number, name, revision, created_at, updated_at, archived_at
		FROM node_slots WHERE id = ? AND user_id = ?`, id, userID).Scan(
		&v.ID, &v.UserID, &v.Number, &v.Name, &v.Revision, &v.CreatedAt, &v.UpdatedAt, &archived)
	if errors.Is(err, sql.ErrNoRows) {
		return NodeSlot{}, ErrNotFound
	}
	if err != nil {
		return NodeSlot{}, err
	}
	v.ArchivedAt = archived.Int64
	return v, nil
}

// PublicNodeIdentities — пакетная подпись для публичной статистики. Один
// запрос на список, без SQL под Registry.mu. Ошибка БД не должна ломать
// публичную выдачу: вызывающий использует fallback на node_id.
func (s *Store) PublicNodeIdentities(nodeIDs []string) (map[string]PublicNodeIdentity, error) {
	out := make(map[string]PublicNodeIdentity, len(nodeIDs))
	if len(nodeIDs) == 0 {
		return out, nil
	}
	uniq := make([]string, 0, len(nodeIDs))
	seen := make(map[string]bool, len(nodeIDs))
	for _, id := range nodeIDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		uniq = append(uniq, id)
	}
	if len(uniq) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(uniq)+1)
	args = append(args, s.legacyUserID)
	for _, id := range uniq {
		args = append(args, id)
	}
	rows, err := s.sql.Query(`SELECT o.node_id, u.username, s.number
		FROM node_owners o
		JOIN users u ON u.id = o.user_id
		JOIN node_slots s ON s.id = o.slot_id
		WHERE o.legacy = 0 AND o.slot_id IS NOT NULL
		  AND o.user_id <> ?
		  AND o.node_id IN (`+placeholders(len(uniq))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var p PublicNodeIdentity
		if err := rows.Scan(&p.NodeID, &p.Username, &p.OwnerNodeNumber); err != nil {
			return nil, err
		}
		out[p.NodeID] = p
	}
	return out, rows.Err()
}

// BackfillNodeSlots — идемпотентная миграция существующих неlegacy-владельцев.
// Номера назначаются детерминированно по registered_at, node_id. Исторические
// технические ID не объединяются: доказать замену задним числом нельзя.
func (s *Store) BackfillNodeSlots() (int, error) {
	tx, err := s.sql.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.Query(`SELECT node_id, user_id FROM node_owners
		WHERE legacy = 0 AND user_id <> ? AND (slot_id IS NULL OR slot_id = 0)
		ORDER BY user_id ASC, registered_at ASC, node_id ASC`, s.legacyUserID)
	if err != nil {
		return 0, err
	}
	type row struct {
		nodeID string
		userID int64
	}
	var list []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.nodeID, &r.userID); err != nil {
			_ = rows.Close()
			return 0, err
		}
		list = append(list, r)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, r := range list {
		slot, err := ensureSlotTx(tx, r.userID, slotNameFromNodeID(r.nodeID))
		if err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`UPDATE node_owners SET slot_id = ? WHERE node_id = ? AND user_id = ? AND legacy = 0`,
			slot.ID, r.nodeID, r.userID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(list), nil
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
