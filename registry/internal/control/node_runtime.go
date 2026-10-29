package control

// Runtime-настройки ноды (ad_tag).
//
// Желаемое значение хранится отдельно от применённого. Успех показывается
// только после подтверждения агента. Старые ноды без строки считаются
// unmanaged: их локальный ad_tag не трогаем и не очищаем миграцией.

import (
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	AdTagUnmanaged = "unmanaged"
	AdTagSet       = "set"
	AdTagClear     = "clear"

	RuntimeUnmanaged   = "unmanaged"
	RuntimePending     = "pending"
	RuntimeApplied     = "applied"
	RuntimeFailed      = "failed"
	RuntimeUnsupported = "unsupported"
)

type NodeRuntimeSettings struct {
	NodeID          string `json:"node_id"`
	AdTagMode       string `json:"ad_tag_mode"`
	AdTagValue      string `json:"ad_tag_value,omitempty"`
	DesiredRevision int64  `json:"desired_revision"`
	AppliedRevision int64  `json:"applied_revision"`
	State           string `json:"state"`
	ErrorCode       string `json:"error_code,omitempty"`
	LastReportAt    int64  `json:"last_report_at,omitempty"`
	UpdatedAt       int64  `json:"updated_at,omitempty"`
}

func scanNodeRuntime(row interface{ Scan(...any) error }) (NodeRuntimeSettings, error) {
	var v NodeRuntimeSettings
	err := row.Scan(&v.NodeID, &v.AdTagMode, &v.AdTagValue, &v.DesiredRevision,
		&v.AppliedRevision, &v.State, &v.ErrorCode, &v.LastReportAt, &v.UpdatedAt)
	return v, err
}

// RuntimeSettingsForNode — текущая запись или unmanaged-дефолт.
func (s *Store) RuntimeSettingsForNode(nodeID string) (NodeRuntimeSettings, error) {
	v, err := scanNodeRuntime(s.sql.QueryRow(`SELECT node_id, ad_tag_mode, ad_tag_value,
		desired_revision, applied_revision, state, error_code, last_report_at, updated_at
		FROM node_runtime_settings WHERE node_id = ?`, nodeID))
	if errors.Is(err, sql.ErrNoRows) {
		return NodeRuntimeSettings{NodeID: nodeID, AdTagMode: AdTagUnmanaged, State: RuntimeUnmanaged}, nil
	}
	if err != nil {
		return NodeRuntimeSettings{}, err
	}
	return v, nil
}

// SetNodeAdTag — новая желаемая ревизия. Вызывается только после проверки
// владельца на уровне server. expectedRevision=0 — первая настройка.
func (s *Store) SetNodeAdTag(userID int64, nodeID, mode, value string, expectedRevision int64) (NodeRuntimeSettings, error) {
	if userID <= 0 || nodeID == "" {
		return NodeRuntimeSettings{}, ErrValidation
	}
	switch mode {
	case AdTagSet:
		value = strings.ToLower(strings.TrimSpace(value))
		raw, err := hex.DecodeString(value)
		if err != nil || len(raw) != 16 || len(value) != 32 {
			return NodeRuntimeSettings{}, fmt.Errorf("%w: ad_tag must be 32 hex characters", ErrValidation)
		}
	case AdTagClear:
		value = ""
	default:
		return NodeRuntimeSettings{}, fmt.Errorf("%w: ad_tag mode must be set or clear", ErrValidation)
	}
	tx, err := s.sql.Begin()
	if err != nil {
		return NodeRuntimeSettings{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var owner int64
	var legacy int
	if err := tx.QueryRow(`SELECT user_id, legacy FROM node_owners WHERE node_id = ?`, nodeID).Scan(&owner, &legacy); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return NodeRuntimeSettings{}, ErrNotFound
		}
		return NodeRuntimeSettings{}, err
	}
	if owner != userID || legacy != 0 {
		return NodeRuntimeSettings{}, ErrNotFound
	}
	current, err := scanNodeRuntime(tx.QueryRow(`SELECT node_id, ad_tag_mode, ad_tag_value,
		desired_revision, applied_revision, state, error_code, last_report_at, updated_at
		FROM node_runtime_settings WHERE node_id = ?`, nodeID))
	if errors.Is(err, sql.ErrNoRows) {
		current = NodeRuntimeSettings{NodeID: nodeID, State: RuntimeUnmanaged}
	} else if err != nil {
		return NodeRuntimeSettings{}, err
	}
	if expectedRevision > 0 && expectedRevision != current.DesiredRevision {
		return NodeRuntimeSettings{}, ErrRevisionConflict
	}
	now := time.Now().Unix()
	revision := current.DesiredRevision + 1
	if _, err := tx.Exec(`INSERT INTO node_runtime_settings
		(node_id, user_id, ad_tag_mode, ad_tag_value, desired_revision, applied_revision, state, error_code, last_report_at, updated_at)
		VALUES (?,?,?,?,?,?,'pending','',?,?)
		ON CONFLICT(node_id) DO UPDATE SET
		  ad_tag_mode = excluded.ad_tag_mode,
		  ad_tag_value = excluded.ad_tag_value,
		  desired_revision = excluded.desired_revision,
		  state = 'pending',
		  error_code = '',
		  updated_at = excluded.updated_at`,
		nodeID, userID, mode, value, revision, current.AppliedRevision, now, now); err != nil {
		return NodeRuntimeSettings{}, err
	}
	if err := tx.Commit(); err != nil {
		return NodeRuntimeSettings{}, err
	}
	return NodeRuntimeSettings{
		NodeID: nodeID, AdTagMode: mode, AdTagValue: value,
		DesiredRevision: revision, AppliedRevision: current.AppliedRevision,
		State: RuntimePending, UpdatedAt: now,
	}, nil
}

// ReportNodeRuntime — подтверждение агента. Применённая ревизия только растёт,
// подтверждение старой ревизии не перетирает новую.
func (s *Store) ReportNodeRuntime(nodeID string, attempted, applied int64, state, errorCode string) (NodeRuntimeSettings, error) {
	switch state {
	case RuntimeApplied, RuntimeFailed, RuntimeUnsupported:
	default:
		return NodeRuntimeSettings{}, fmt.Errorf("%w: invalid runtime state", ErrValidation)
	}
	tx, err := s.sql.Begin()
	if err != nil {
		return NodeRuntimeSettings{}, err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := scanNodeRuntime(tx.QueryRow(`SELECT node_id, ad_tag_mode, ad_tag_value,
		desired_revision, applied_revision, state, error_code, last_report_at, updated_at
		FROM node_runtime_settings WHERE node_id = ?`, nodeID))
	if errors.Is(err, sql.ErrNoRows) {
		return NodeRuntimeSettings{}, ErrNotFound
	}
	if err != nil {
		return NodeRuntimeSettings{}, err
	}
	if attempted != current.DesiredRevision {
		// Старый агент подтверждает предыдущую ревизию: применяемую часть
		// можно поднять, но pending новой ревизии не снимаем.
		if applied > current.AppliedRevision && applied <= current.DesiredRevision {
			if _, err := tx.Exec(`UPDATE node_runtime_settings SET applied_revision = ?, last_report_at = ?, updated_at = ? WHERE node_id = ?`,
				applied, time.Now().Unix(), time.Now().Unix(), nodeID); err != nil {
				return NodeRuntimeSettings{}, err
			}
		}
		if err := tx.Commit(); err != nil {
			return NodeRuntimeSettings{}, err
		}
		return s.RuntimeSettingsForNode(nodeID)
	}
	now := time.Now().Unix()
	nextApplied := current.AppliedRevision
	if applied > nextApplied && applied <= current.DesiredRevision {
		nextApplied = applied
	}
	if _, err := tx.Exec(`UPDATE node_runtime_settings
		SET applied_revision = ?, state = ?, error_code = ?, last_report_at = ?, updated_at = ?
		WHERE node_id = ?`, nextApplied, state, errorCode, now, now, nodeID); err != nil {
		return NodeRuntimeSettings{}, err
	}
	if err := tx.Commit(); err != nil {
		return NodeRuntimeSettings{}, err
	}
	return s.RuntimeSettingsForNode(nodeID)
}
