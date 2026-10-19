package control

// Контрольная БД мультиарендного пула (docs/api-contract.md §1):
// пользователи, сессии, authkeys, владельцы нод, квотные резервы.
//
// Fail-closed по контракту: Open возвращает ошибку → вызывающий (сервер)
// отвечает 503 на user-API и на любую регистрацию, требующую владельца;
// legacy-режим ([users] enabled=false) БД вообще не открывает.
// Схема — CREATE TABLE IF NOT EXISTS внутри Open (миграции встроены).

import (
	"database/sql"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"
)

// Сентинельные ошибки: сервер маппит их на HTTP-коды контракта §5.
var (
	ErrValidation = errors.New("validation failed")
	ErrDuplicate  = errors.New("already exists")
	// ErrDuplicateName — имя профиля занято (коллизия без учёта регистра).
	// Отдельно от ErrDuplicate: writeControlErr маппит его в 409 duplicate_name
	// с понятным русским текстом, а не «username already exists».
	ErrDuplicateName    = errors.New("duplicate name")
	ErrNotFound         = errors.New("not found")
	ErrBadCredentials   = errors.New("bad credentials")
	ErrUserDisabled     = errors.New("user disabled")
	ErrExpired          = errors.New("expired")
	ErrQuotaExceeded    = errors.New("quota exceeded")
	ErrRateLimited      = errors.New("rate limited")
	ErrCooldown         = errors.New("cooldown active")
	ErrCreateInProgress = errors.New("create already in progress")
	ErrOwnerMismatch    = errors.New("owner mismatch")
	// ErrNodeDeleting — нода удалена владельцем; агент не должен возвращать её в пул.
	ErrNodeDeleting    = errors.New("node is marked for deletion")
	ErrProfileInUse    = errors.New("profile in use")
	ErrLastAdmin       = errors.New("cannot disable last active admin")
	ErrLegacyProtected = errors.New("legacy user is protected")
	// Приглашения (contract §2): различимые коды отказа регистрации.
	ErrInviteRequired = errors.New("invite required")
	ErrInviteInvalid  = errors.New("invite invalid")
	ErrInviteExpired  = errors.New("invite expired")
	ErrInviteUsed     = errors.New("invite used")
	ErrInviteRevoked  = errors.New("invite revoked")
)

// LegacyUsername — системный пользователь для нод, зарегистрированных
// до включения user-режима (миграция §4). Создаётся Open'ом, защищён от
// блокировки через API.
const LegacyUsername = "legacy"

type Store struct {
	sql          *sql.DB
	legacyUserID int64
}

// Open — открыть (создать) контрольную БД, гарантировать схему и наличие
// системного legacy-пользователя. Ошибка → (*nil, err): пул обязан отвечать
// fail-closed, «тихая деградация» как у history здесь запрещена контрактом.
func Open(path string, legacyMaxNodes int) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("control: empty db path")
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, fmt.Errorf("control: open %s: %w", path, err)
	}
	// Одно соединение: сериализует писателей (транзакция квоты атомарна
	// по определению), устраняет busy-гонки между соединениями пула.
	db.SetMaxOpenConns(1)
	schema := `
CREATE TABLE IF NOT EXISTS users (
 id INTEGER PRIMARY KEY,
 username TEXT UNIQUE NOT NULL,
 password_hash TEXT NOT NULL,
 role TEXT NOT NULL CHECK(role IN ('admin','user')),
 max_nodes INTEGER NOT NULL DEFAULT 0,
 disabled INTEGER NOT NULL DEFAULT 0,
 created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
 token_hash TEXT PRIMARY KEY,
 user_id INTEGER NOT NULL REFERENCES users(id),
 expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS authkeys (
 key_hash TEXT PRIMARY KEY,
 user_id INTEGER NOT NULL REFERENCES users(id),
 prefix TEXT NOT NULL,
 created_at INTEGER NOT NULL,
 revoked_at INTEGER
);
CREATE TABLE IF NOT EXISTS node_owners (
 node_id TEXT PRIMARY KEY,
 user_id INTEGER NOT NULL REFERENCES users(id),
 registered_at INTEGER NOT NULL,
 legacy INTEGER NOT NULL DEFAULT 0,
 deleting INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS quota_reservations (
 node_id TEXT UNIQUE,
 user_id INTEGER NOT NULL REFERENCES users(id),
 created_at INTEGER NOT NULL
);
-- ── M2: личный Beget, профили, платные операции (beget-provisioning.md §1) ──
CREATE TABLE IF NOT EXISTS beget_links (
 id INTEGER PRIMARY KEY,
 user_id INTEGER UNIQUE NOT NULL REFERENCES users(id),
 login_enc BLOB NOT NULL,
 password_enc BLOB NOT NULL,
 ssh_key_id INTEGER NOT NULL DEFAULT 0,
 ssh_pub TEXT NOT NULL DEFAULT '',
 ssh_priv_enc BLOB NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL,
 verified_at INTEGER NOT NULL DEFAULT 0,
 last_error TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS profiles (
 id INTEGER PRIMARY KEY,
 name TEXT UNIQUE NOT NULL,
 description TEXT NOT NULL DEFAULT '',
 published INTEGER NOT NULL DEFAULT 0,
 created_by INTEGER NOT NULL DEFAULT 0,
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS profile_versions (
 id INTEGER PRIMARY KEY,
 profile_id INTEGER NOT NULL REFERENCES profiles(id),
 version INTEGER NOT NULL,
 playbook TEXT NOT NULL,
 vars_schema TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL,
 UNIQUE(profile_id, version)
);
CREATE TABLE IF NOT EXISTS operations (
 id INTEGER PRIMARY KEY,
 user_id INTEGER NOT NULL,
 node_id TEXT NOT NULL DEFAULT '',
 kind TEXT NOT NULL,
 state TEXT NOT NULL,
 idem_key TEXT UNIQUE NOT NULL,
 provider_vps_id TEXT NOT NULL DEFAULT '',
 provider_op_id TEXT NOT NULL DEFAULT '',
 region TEXT NOT NULL DEFAULT '',
 config_id TEXT NOT NULL DEFAULT '',
 profile_id INTEGER NOT NULL DEFAULT 0,
 request_json TEXT NOT NULL DEFAULT '',
 last_error TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_operations_user_kind_created ON operations(user_id, kind, created_at);
-- plan-ui-batch.md Фаза 3: пошаговый журнал установки (раскрывающийся лог шага).
CREATE TABLE IF NOT EXISTS operation_steps (
 id INTEGER PRIMARY KEY,
 operation_id INTEGER NOT NULL,
 idx INTEGER NOT NULL,
 name TEXT NOT NULL DEFAULT '',
 status TEXT NOT NULL,
 started_at INTEGER NOT NULL,
 finished_at INTEGER NOT NULL DEFAULT 0,
 output TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_operation_steps_op ON operation_steps(operation_id, idx);
CREATE TABLE IF NOT EXISTS cloud_resources (
 vps_id TEXT PRIMARY KEY,
 user_id INTEGER NOT NULL,
 node_id TEXT NOT NULL DEFAULT '',
 region TEXT NOT NULL DEFAULT '',
 config_id TEXT NOT NULL DEFAULT '',
 state TEXT NOT NULL,
 created_at INTEGER NOT NULL,
 deleted_at INTEGER NOT NULL DEFAULT 0
);
-- ── invitations-migration-contract.md §2: одноразовые приглашения ──
CREATE TABLE IF NOT EXISTS invites (
 id INTEGER PRIMARY KEY,
 token_hash TEXT UNIQUE NOT NULL,
 prefix TEXT NOT NULL,
 created_by INTEGER NOT NULL DEFAULT 0,
 created_at INTEGER NOT NULL,
 expires_at INTEGER NOT NULL,
 used_by INTEGER,
 used_at INTEGER,
 revoked_at INTEGER,
 max_uses INTEGER NOT NULL DEFAULT 1,
 used_count INTEGER NOT NULL DEFAULT 0
);
-- ── §3: durable-настройки (переключатель «Разрешить общий секрет») ──
CREATE TABLE IF NOT EXISTS settings (
 key TEXT PRIMARY KEY,
 value TEXT NOT NULL
);
-- ── addendum-manual-servers.md §2: свои серверы (без провайдера) ──
CREATE TABLE IF NOT EXISTS manual_hosts (
 id INTEGER PRIMARY KEY,
 user_id INTEGER NOT NULL REFERENCES users(id),
 label TEXT NOT NULL,
 host TEXT NOT NULL,
 port INTEGER NOT NULL DEFAULT 22,
 ssh_user TEXT NOT NULL DEFAULT 'root',
 auth_type TEXT NOT NULL CHECK(auth_type IN ('password','key')),
 secret_enc BLOB NOT NULL,
 created_at INTEGER NOT NULL,
 verified_at INTEGER NOT NULL DEFAULT 0,
 last_error TEXT NOT NULL DEFAULT ''
);
-- ── claim-on-first-register (manual installs): секрет ожидает /register ──
-- node_id manual-сервера неизвестен до первой регистрации агента, поэтому
-- место в квоте держит pending-секрет, а node_owners пишется при claim.
CREATE TABLE IF NOT EXISTS pending_node_secrets (
 secret_hash TEXT PRIMARY KEY,
 user_id INTEGER NOT NULL REFERENCES users(id),
 operation_id INTEGER NOT NULL,
 label TEXT NOT NULL DEFAULT '',
 secret_enc TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_pending_node_secrets_created ON pending_node_secrets(created_at);
-- ── hosting-blueprints stage-04 (03-cabinet-api.md §3, 04 §2): подключения
-- провайдеров хостинга + снимки баланса (CAS по credential_revision) ──
CREATE TABLE IF NOT EXISTS hosting_connections (
 id INTEGER PRIMARY KEY,
 user_id INTEGER NOT NULL REFERENCES users(id),
 provider TEXT NOT NULL CHECK(provider IN ('nodehost','beget')),
 label TEXT NOT NULL,
 credentials_enc TEXT NOT NULL,
 credential_revision INTEGER NOT NULL DEFAULT 1,
 revision INTEGER NOT NULL DEFAULT 1,
 verified_at INTEGER NOT NULL DEFAULT 0,
 capabilities_json TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_hosting_connections_user ON hosting_connections(user_id);
CREATE TABLE IF NOT EXISTS connection_balances (
 connection_id INTEGER PRIMARY KEY REFERENCES hosting_connections(id),
 user_id INTEGER NOT NULL,
 credential_revision INTEGER NOT NULL,
 amount TEXT,
 currency TEXT NOT NULL DEFAULT '',
 scope TEXT NOT NULL DEFAULT '',
 checked_at INTEGER NOT NULL DEFAULT 0,
 last_attempt_at INTEGER NOT NULL DEFAULT 0,
 safe_error TEXT NOT NULL DEFAULT ''
);
-- ── hosting-blueprints stage-05 (03-cabinet-api.md §4-§5, 04 §2): управляемые
-- облачные ресурсы + политика управления нодой.
-- Имя cloud_resources2 (НЕ legacy cloud_resources): legacy-таблица
-- (vps_id TEXT PK) — часть денежного квотного контура
-- ReserveCreateOperation/UsedSlots и её on-disk формат менять нельзя;
-- новая форма (surrogate PK, связь с hosting_connections) живёт параллельно.
CREATE TABLE IF NOT EXISTS cloud_resources2 (
 id INTEGER PRIMARY KEY,
 user_id INTEGER NOT NULL REFERENCES users(id),
 connection_id INTEGER NOT NULL REFERENCES hosting_connections(id),
 provider TEXT NOT NULL,
 external_id TEXT NOT NULL DEFAULT '',
 node_id TEXT NOT NULL DEFAULT '',
 label TEXT NOT NULL DEFAULT '',
 state TEXT NOT NULL,
 revision INTEGER NOT NULL DEFAULT 1,
 ip TEXT NOT NULL DEFAULT '',
 secrets_enc TEXT NOT NULL DEFAULT '',
 replaces_resource_id INTEGER,
 replaced_by_resource_id INTEGER,
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_cloud_resources2_user ON cloud_resources2(user_id);
-- H7/H8 guard scans by (user, node, state); keep it index-backed.
CREATE INDEX IF NOT EXISTS idx_cloud_resources2_user_node ON cloud_resources2(user_id, node_id, state);
-- Внешний ID уникален в рамках подключения; пустой (= ещё не выделен
-- провайдером) не участвует — частичный UNIQUE-индекс.
CREATE UNIQUE INDEX IF NOT EXISTS ux_cloud_resources2_conn_ext
 ON cloud_resources2(connection_id, external_id) WHERE external_id <> '';
CREATE TABLE IF NOT EXISTS node_management (
 node_id TEXT PRIMARY KEY,
 user_id INTEGER NOT NULL REFERENCES users(id),
 mode TEXT NOT NULL DEFAULT 'manual' CHECK(mode IN ('manual','profile')),
 profile_version_id INTEGER,
 inputs_json TEXT NOT NULL DEFAULT '',
 process_current_ban INTEGER NOT NULL DEFAULT 0,
 revision INTEGER NOT NULL DEFAULT 1,
 current_resource_id INTEGER,
 updated_at INTEGER NOT NULL
);
-- ── hosting-blueprints stage-08b-i (04 §2/§4-§5): durable run engine ──
-- ── fleet autopilot: batch of managed cloud resources + install/policy ──
CREATE TABLE IF NOT EXISTS fleets (
 id INTEGER PRIMARY KEY,
 user_id INTEGER NOT NULL REFERENCES users(id),
 connection_id INTEGER NOT NULL REFERENCES hosting_connections(id),
 count INTEGER NOT NULL,
 auto_replace INTEGER NOT NULL DEFAULT 0,
 profile_version_id INTEGER NOT NULL,
 configuration_json TEXT NOT NULL DEFAULT '',
 fleet_key TEXT NOT NULL,
 state TEXT NOT NULL,
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL,
 UNIQUE(user_id, fleet_key)
);
CREATE INDEX IF NOT EXISTS idx_fleets_user ON fleets(user_id, id);
CREATE TABLE IF NOT EXISTS fleet_items (
 id INTEGER PRIMARY KEY,
 fleet_id INTEGER NOT NULL REFERENCES fleets(id),
 idx INTEGER NOT NULL,
 label TEXT NOT NULL,
 resource_id INTEGER,
 node_id TEXT NOT NULL DEFAULT '',
 state TEXT NOT NULL,
 last_error TEXT NOT NULL DEFAULT '',
 updated_at INTEGER NOT NULL,
 UNIQUE(fleet_id, idx)
);
CREATE INDEX IF NOT EXISTS idx_fleet_items_fleet ON fleet_items(fleet_id, idx);
CREATE TABLE IF NOT EXISTS profile_runs (
 id INTEGER PRIMARY KEY,
 user_id INTEGER NOT NULL REFERENCES users(id),
 profile_version_id INTEGER NOT NULL,
 profile_id INTEGER NOT NULL,
 node_id TEXT NOT NULL DEFAULT '',
 entry TEXT NOT NULL CHECK(entry IN ('initial','observe')),
 state TEXT NOT NULL,
 revision INTEGER NOT NULL DEFAULT 1,
 inputs_json TEXT NOT NULL DEFAULT '{}',
 vars_json TEXT NOT NULL DEFAULT '{}',
 current_resource_id INTEGER,
 operation_id INTEGER NOT NULL DEFAULT 0,
 policy_id INTEGER NOT NULL DEFAULT 0,
 policy_revision INTEGER NOT NULL DEFAULT 0,
 next_wake_at INTEGER NOT NULL DEFAULT 0,
 cancel_requested INTEGER NOT NULL DEFAULT 0,
 last_error TEXT NOT NULL DEFAULT '',
 lease_generation INTEGER NOT NULL DEFAULT 0,
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_profile_runs_user ON profile_runs(user_id, id);
CREATE INDEX IF NOT EXISTS idx_profile_runs_claim ON profile_runs(state, next_wake_at);
CREATE TABLE IF NOT EXISTS block_checkpoints (
 id INTEGER PRIMARY KEY,
 run_id INTEGER NOT NULL REFERENCES profile_runs(id),
 node_id TEXT NOT NULL,
 invocation INTEGER NOT NULL DEFAULT 0,
 state TEXT NOT NULL,
 attempt INTEGER NOT NULL DEFAULT 0,
 output_json TEXT NOT NULL DEFAULT '',
 error TEXT NOT NULL DEFAULT '',
 lease_generation INTEGER NOT NULL DEFAULT 0,
 next_attempt_at INTEGER NOT NULL DEFAULT 0,
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL,
 UNIQUE(run_id, node_id, invocation)
);
CREATE INDEX IF NOT EXISTS idx_block_checkpoints_run ON block_checkpoints(run_id, id);
CREATE TABLE IF NOT EXISTS ban_episodes (
 id INTEGER PRIMARY KEY,
 node_id TEXT NOT NULL,
 ip TEXT NOT NULL,
 episode_id TEXT UNIQUE NOT NULL,
 created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_ban_episodes_node ON ban_episodes(node_id, created_at);
CREATE TABLE IF NOT EXISTS ban_deliveries (
 id INTEGER PRIMARY KEY,
 policy_node_id TEXT NOT NULL,
 episode_id TEXT NOT NULL,
 run_id INTEGER,
 state TEXT NOT NULL,
 created_at INTEGER NOT NULL,
 UNIQUE(policy_node_id, episode_id)
);`
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("control: schema %s: %w", path, err)
	}
	// stage-08b-i L1: ack operation id for run DTO replay.
	if err := ensureColumn(db, "profile_runs", "operation_id", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("control: migrate profile_runs.operation_id: %w", err)
	}
	// H7: at most one non-terminal run per policy. policy_id=0 (initial runs
	// without a management policy) is excluded.
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS ux_profile_runs_policy_active
		ON profile_runs(policy_id) WHERE policy_id > 0 AND state IN
		('pending','running','waiting_retry','reconciling','needs_attention','paused')`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("control: index profile_runs policy: %w", err)
	}
	// Миграция существующих БД: CREATE TABLE IF NOT EXISTS не добавит колонку
	// в уже созданную node_owners (M1 закладка delete-маркера).
	if err := ensureColumn(db, "node_owners", "deleting", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("control: migrate node_owners.deleting: %w", err)
	}
	if err := ensureColumn(db, "node_owners", "released_user_id", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("control: migrate node_owners.released_user_id: %w", err)
	}
	// Персональный секрет ноды (contract §1): только sha256 + префикс.
	if err := ensureColumn(db, "node_owners", "node_secret_hash", "TEXT NOT NULL DEFAULT ''"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("control: migrate node_owners.node_secret_hash: %w", err)
	}
	if err := ensureColumn(db, "node_owners", "node_secret_prefix", "TEXT NOT NULL DEFAULT ''"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("control: migrate node_owners.node_secret_prefix: %w", err)
	}
	// DEF-A: восстановимая (AES-GCM) копия персонального секрета — чтобы
	// registry-initiated retry-install отрендерил ту же команду с реальным
	// токеном. Хеш выше остаётся источником истины аутентификации.
	if err := ensureColumn(db, "node_owners", "node_secret_enc", "TEXT NOT NULL DEFAULT ''"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("control: migrate node_owners.node_secret_enc: %w", err)
	}
	// Приглашения на N регистраций (addendum-profiles-vps.md §2):
	// максимум использований + счётчик; для существующих строк дефолт 1/0.
	if err := ensureColumn(db, "invites", "max_uses", "INTEGER NOT NULL DEFAULT 1"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("control: migrate invites.max_uses: %w", err)
	}
	if err := ensureColumn(db, "invites", "used_count", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("control: migrate invites.used_count: %w", err)
	}
	// Профиль установки операции (addendum §7.2): retry-install должен повторять
	// ТОТ ЖЕ профиль, что выбрал оператор. 0 = не задан (legacy) → дефолт.
	if err := ensureColumn(db, "operations", "profile_id", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("control: migrate operations.profile_id: %w", err)
	}
	// Provider-side operation id облачного create: персистится ДО поллинга,
	// чтобы рестарт мог возобновить сверку без повторного платного Create.
	if err := ensureColumn(db, "operations", "provider_op_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("control: migrate operations.provider_op_id: %w", err)
	}
	// Свои серверы (addendum-manual-servers.md §2): предсозданная нода и
	// операция установки привязываются к записи после accept.
	if err := ensureColumn(db, "manual_hosts", "node_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("control: migrate manual_hosts.node_id: %w", err)
	}
	if err := ensureColumn(db, "manual_hosts", "operation_id", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("control: migrate manual_hosts.operation_id: %w", err)
	}
	if err := ensureColumn(db, "manual_hosts", "install_key", "TEXT NOT NULL DEFAULT ''"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("control: migrate manual_hosts.install_key: %w", err)
	}
	if err := migrateManualInstallKeys(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("control: backfill manual_hosts.install_key: %w", err)
	}
	// B2 (QA round 7): UNIQUE(user_id, host, port) — один физический сервер =
	// одно место в квоте. Безопасная миграция существующих БД: сначала схлопываем
	// уже накопленные дубли (CREATE UNIQUE INDEX иначе упал бы), затем создаём
	// индекс. Индекс, а не табличная UNIQUE-константа, потому что ADD CONSTRAINT
	// в SQLite недоступен; семантика проверки идентична.
	if err := migrateManualHostUniqueness(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("control: migrate manual_hosts uniqueness: %w", err)
	}
	// hosting-blueprints stage-08a (05 §2/§3, 04 §2): graph profile versions.
	// Additive, idempotent; existing text rows keep format='text' and empty graph.
	for _, col := range []struct{ name, ddl string }{
		{"format", "TEXT NOT NULL DEFAULT 'text'"},
		{"blueprint_json", "TEXT NOT NULL DEFAULT ''"},
		{"schema_version", "INTEGER NOT NULL DEFAULT 0"},
		{"content_hash", "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := ensureColumn(db, "profile_versions", col.name, col.ddl); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("control: migrate profile_versions.%s: %w", col.name, err)
		}
	}
	// stage-08b-i: enable-profile watermark (ban episodes older than this are
	// historical and not processed by default).
	if err := ensureColumn(db, "node_management", "activated_at", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("control: migrate node_management.activated_at: %w", err)
	}
	// cloud truthfulness: last safe failure code + last create/install operation.
	if err := ensureColumn(db, "cloud_resources2", "last_error", "TEXT NOT NULL DEFAULT ''"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("control: migrate cloud_resources2.last_error: %w", err)
	}
	if err := ensureColumn(db, "cloud_resources2", "last_operation_id", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("control: migrate cloud_resources2.last_operation_id: %w", err)
	}
	s := &Store{sql: db}
	if err := s.ensureLegacyUser(legacyMaxNodes); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("control: legacy user: %w", err)
	}
	return s, nil
}

func (s *Store) Close() { _ = s.sql.Close() }

// ensureColumn добавляет колонку, если её ещё нет (идемпотентная мини-миграция
// поверх CREATE TABLE IF NOT EXISTS, который существующие таблицы не меняет).
func ensureColumn(db *sql.DB, table, column, ddl string) error {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			cid, notNull, pk int
			name, colType    string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + ddl)
	return err
}

// migrateManualHostUniqueness схлопывает дубликаты manual_hosts по
// (user_id, host, port) и создаёт UNIQUE-индекс. Для каждой группы оставляем
// одну строку: предпочитаем запись с операцией (operation_id != 0), иначе самую
// раннюю. Лишние строки удаляем вместе с их резервами (node_owners +
// quota_reservations): дублирующая запись держала второе место квоты за тот же
// физический хост. Идемпотентно: при отсутствии дублей индекс просто создаётся.
func migrateManualHostUniqueness(db *sql.DB) error {
	rows, err := db.Query(`SELECT id, node_id FROM manual_hosts WHERE id NOT IN (
		SELECT id FROM (
			SELECT id, ROW_NUMBER() OVER (
				PARTITION BY user_id, host, port
				ORDER BY (operation_id != 0) DESC, id ASC
			) AS rn FROM manual_hosts
		) WHERE rn = 1)`)
	if err != nil {
		return err
	}
	type dupRow struct {
		id     int64
		nodeID string
	}
	var dups []dupRow
	for rows.Next() {
		var d dupRow
		if err := rows.Scan(&d.id, &d.nodeID); err != nil {
			_ = rows.Close()
			return err
		}
		dups = append(dups, d)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	for _, d := range dups {
		if d.nodeID != "" {
			if _, err := db.Exec(`DELETE FROM node_owners WHERE node_id = ?`, d.nodeID); err != nil {
				return err
			}
			if _, err := db.Exec(`DELETE FROM quota_reservations WHERE node_id = ?`, d.nodeID); err != nil {
				return err
			}
		}
		if _, err := db.Exec(`DELETE FROM manual_hosts WHERE id = ?`, d.id); err != nil {
			return err
		}
	}
	_, err = db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS ux_manual_hosts_user_host_port
		ON manual_hosts(user_id, host, port)`)
	return err
}

// LegacyUserID — id системного legacy-пользователя (0 = БД не открыта).
func (s *Store) LegacyUserID() int64 { return s.legacyUserID }
