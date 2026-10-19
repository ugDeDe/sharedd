package control

// Fleet autopilot (docs/specs/hosting-blueprints/fleet-autopilot.md):
// a user-owned batch of cloud resources that are created, installed and
// (optionally) armed with the system nodehost replacement graph in one go.
// Storage is intentionally plain: fleets own the immutable request snapshot,
// fleet_items own per-item progress. No money state lives here.

import (
	"database/sql"
	"errors"
	"time"
)

const (
	FleetRunning   = "running"
	FleetSucceeded = "succeeded"
	FleetPartial   = "partial"
	FleetFailed    = "failed"
)

const (
	FleetItemPending    = "pending"
	FleetItemCreating   = "creating"
	FleetItemInstalling = "installing"
	FleetItemAdmitted   = "admitted"
	FleetItemError      = "error"
)

// Fleet is the persisted autopilot request + aggregate state.
type Fleet struct {
	ID                int64  `json:"id"`
	UserID            int64  `json:"user_id"`
	ConnectionID      int64  `json:"connection_id"`
	Count             int    `json:"count"`
	AutoReplace       bool   `json:"auto_replace"`
	ProfileVersionID  int64  `json:"profile_version_id"`
	ConfigurationJSON string `json:"-"`
	FleetKey          string `json:"-"`
	State             string `json:"state"`
	CreatedAt         int64  `json:"created_at"`
	UpdatedAt         int64  `json:"updated_at"`
}

// FleetItem is one slot in a fleet. Idx is 1-based and matches the "-01"
// suffix of the label.
type FleetItem struct {
	ID         int64  `json:"id"`
	FleetID    int64  `json:"fleet_id"`
	Idx        int    `json:"idx"`
	Label      string `json:"label"`
	ResourceID int64  `json:"resource_id"`
	NodeID     string `json:"node_id"`
	State      string `json:"state"`
	LastError  string `json:"last_error"`
	UpdatedAt  int64  `json:"updated_at"`
}

func validFleetState(s string) bool {
	switch s {
	case FleetRunning, FleetSucceeded, FleetPartial, FleetFailed:
		return true
	}
	return false
}

func validFleetItemState(s string) bool {
	switch s {
	case FleetItemPending, FleetItemCreating, FleetItemInstalling, FleetItemAdmitted, FleetItemError:
		return true
	}
	return false
}

// CreateFleet persists a fleet + its empty items atomically. fleet_key is
// unique per user; a replay returns the existing fleet. Labels must be
// precomputed by the caller (one per item, in idx order).
func (s *Store) CreateFleet(f Fleet, labels []string) (Fleet, bool, error) {
	if f.UserID == 0 || f.ConnectionID == 0 || f.Count < 1 || f.Count > 20 ||
		f.ProfileVersionID <= 0 || f.FleetKey == "" || f.ConfigurationJSON == "" ||
		len(labels) != f.Count || !validFleetState(f.State) {
		return Fleet{}, false, ErrValidation
	}
	if existing, err := s.FleetByKey(f.UserID, f.FleetKey); err == nil {
		return existing, false, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Fleet{}, false, err
	}
	tx, err := s.sql.Begin()
	if err != nil {
		return Fleet{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().Unix()
	ban := 0
	if f.AutoReplace {
		ban = 1
	}
	res, err := tx.Exec(
		`INSERT INTO fleets
		 (user_id, connection_id, count, auto_replace, profile_version_id, configuration_json, fleet_key, state, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		f.UserID, f.ConnectionID, f.Count, ban, f.ProfileVersionID, f.ConfigurationJSON, f.FleetKey, f.State, now, now)
	if err != nil {
		// Lost a race with a concurrent replay of the same Idempotency-Key.
		if existing, lookupErr := s.FleetByKey(f.UserID, f.FleetKey); lookupErr == nil {
			return existing, false, nil
		}
		return Fleet{}, false, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Fleet{}, false, err
	}
	for i, label := range labels {
		if _, err := tx.Exec(
			`INSERT INTO fleet_items (fleet_id, idx, label, state, updated_at) VALUES (?,?,?,?,?)`,
			id, i+1, label, FleetItemPending, now); err != nil {
			return Fleet{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Fleet{}, false, err
	}
	f.ID, f.CreatedAt, f.UpdatedAt = id, now, now
	return f, true, nil
}

// FleetByKey — usage of an Idempotency-Key in the user's namespace.
func (s *Store) FleetByKey(userID int64, key string) (Fleet, error) {
	f, err := scanFleet(s.sql.QueryRow(
		`SELECT id, user_id, connection_id, count, auto_replace, profile_version_id, configuration_json, fleet_key, state, created_at, updated_at
		 FROM fleets WHERE user_id = ? AND fleet_key = ?`, userID, key))
	if errors.Is(err, sql.ErrNoRows) {
		return Fleet{}, ErrNotFound
	}
	return f, err
}

// Fleet — fleet by id (worker-internal; user routes use FleetOwned).
func (s *Store) Fleet(id int64) (Fleet, error) {
	f, err := scanFleet(s.sql.QueryRow(
		`SELECT id, user_id, connection_id, count, auto_replace, profile_version_id, configuration_json, fleet_key, state, created_at, updated_at
		 FROM fleets WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Fleet{}, ErrNotFound
	}
	return f, err
}

// FleetOwned — fleet of the caller; foreign/unknown → ErrNotFound.
func (s *Store) FleetOwned(id, userID int64) (Fleet, error) {
	f, err := scanFleet(s.sql.QueryRow(
		`SELECT id, user_id, connection_id, count, auto_replace, profile_version_id, configuration_json, fleet_key, state, created_at, updated_at
		 FROM fleets WHERE id = ? AND user_id = ?`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return Fleet{}, ErrNotFound
	}
	return f, err
}

func scanFleet(row interface{ Scan(...any) error }) (Fleet, error) {
	var f Fleet
	var ban int64
	err := row.Scan(&f.ID, &f.UserID, &f.ConnectionID, &f.Count, &ban, &f.ProfileVersionID,
		&f.ConfigurationJSON, &f.FleetKey, &f.State, &f.CreatedAt, &f.UpdatedAt)
	if err != nil {
		return Fleet{}, err
	}
	f.AutoReplace = ban != 0
	return f, nil
}

// FleetsByUser — newest first; deleted fleets are not a concept.
func (s *Store) FleetsByUser(userID int64) ([]Fleet, error) {
	rows, err := s.sql.Query(
		`SELECT id, user_id, connection_id, count, auto_replace, profile_version_id, configuration_json, fleet_key, state, created_at, updated_at
		 FROM fleets WHERE user_id = ? ORDER BY id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Fleet{}
	for rows.Next() {
		f, err := scanFleet(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// FleetItems — items ordered by idx (1-based).
func (s *Store) FleetItems(fleetID int64) ([]FleetItem, error) {
	rows, err := s.sql.Query(
		`SELECT id, fleet_id, idx, label, resource_id, node_id, state, last_error, updated_at
		 FROM fleet_items WHERE fleet_id = ? ORDER BY idx ASC`, fleetID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []FleetItem{}
	for rows.Next() {
		var it FleetItem
		var resID sql.NullInt64
		if err := rows.Scan(&it.ID, &it.FleetID, &it.Idx, &it.Label, &resID, &it.NodeID, &it.State, &it.LastError, &it.UpdatedAt); err != nil {
			return nil, err
		}
		it.ResourceID = resID.Int64
		out = append(out, it)
	}
	return out, rows.Err()
}

// SetFleetState — aggregate state of a fleet.
func (s *Store) SetFleetState(id int64, state string) error {
	if !validFleetState(state) {
		return ErrValidation
	}
	_, err := s.sql.Exec(`UPDATE fleets SET state = ?, updated_at = ? WHERE id = ?`, state, time.Now().Unix(), id)
	return err
}

// SetFleetItemResource — the paid create was accepted: remember the resource
// row and move the item to creating.
func (s *Store) SetFleetItemResource(fleetID int64, idx int, resourceID int64) error {
	if resourceID <= 0 {
		return ErrValidation
	}
	_, err := s.sql.Exec(
		`UPDATE fleet_items SET resource_id = ?, state = ?, last_error = '', updated_at = ? WHERE fleet_id = ? AND idx = ?`,
		resourceID, FleetItemCreating, time.Now().Unix(), fleetID, idx)
	return err
}

// SetFleetItemState — progress/terminal state. Empty nodeID keeps the current
// binding; lastError replaces the previous value.
func (s *Store) SetFleetItemState(fleetID int64, idx int, state, nodeID, lastError string) error {
	if !validFleetItemState(state) {
		return ErrValidation
	}
	_, err := s.sql.Exec(
		`UPDATE fleet_items SET state = ?, node_id = CASE WHEN ? = '' THEN node_id ELSE ? END,
		 last_error = ?, updated_at = ? WHERE fleet_id = ? AND idx = ?`,
		state, nodeID, nodeID, lastError, time.Now().Unix(), fleetID, idx)
	return err
}
