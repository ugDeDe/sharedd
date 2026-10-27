package control

// Профили установки (docs/specs/beget-provisioning.md §5, addendum §3): профиль =
// имя/описание/published + одна строка контента (шаги shell-команд в формате
// mtplane + vars_schema, либо blueprint-граф). Версий больше нет: правки
// обновляют эту строку in place. Шаги выполняются по SSH на VPS пользователя.
// Пользователь выбирает только published-профили.
//
// Имя колонки БД оставлено историческим (`playbook`) — переименование
// потребовало бы миграции существующих баз; наружу и в Go поле называется
// Steps.

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"sharedd/registry/internal/config"
)

// Profile — именованный набор версий шагов.
type Profile struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Published   bool   `json:"published"`
	CreatedBy   int64  `json:"created_by"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

// ProfileVersion — конкретная версия шагов + таблица переменных. Format
// пуст/"text" для legacy и текст-версий, "graph" для blueprint-версий
// (hosting-blueprints stage-08a); Blueprint хранит канонический JSON графа.
type ProfileVersion struct {
	ID            int64  `json:"id"`
	ProfileID     int64  `json:"profile_id"`
	Version       int    `json:"version"`
	Steps         string `json:"steps"`
	VarsSchema    string `json:"vars_schema"`
	Format        string `json:"format,omitempty"`
	Blueprint     string `json:"blueprint,omitempty"`
	SchemaVersion int    `json:"schema_version,omitempty"`
	ContentHash   string `json:"content_hash,omitempty"`
	CreatedAt     int64  `json:"created_at"`
}

// FormatText / FormatGraph — значения profile_versions.format.
const (
	FormatText  = "text"
	FormatGraph = "graph"
)

const profileVersionCols = `id, profile_id, version, playbook, vars_schema, created_at, format, blueprint_json, schema_version, content_hash`

const profileCols = `id, name, description, published, created_by, created_at, updated_at`

func scanProfile(row interface{ Scan(...any) error }) (Profile, error) {
	var p Profile
	var published int
	err := row.Scan(&p.ID, &p.Name, &p.Description, &published, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt)
	p.Published = published != 0
	return p, err
}

// CreateProfile создаёт черновик профиля (published=0).
func (s *Store) CreateProfile(name, description string, createdBy int64) (Profile, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Profile{}, fmt.Errorf("%w: profile name required", ErrValidation)
	}
	now := time.Now().Unix()
	res, err := s.sql.Exec(
		`INSERT INTO profiles (name, description, published, created_by, created_at, updated_at) VALUES (?,?,0,?,?,?)`,
		name, description, createdBy, now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return Profile{}, ErrDuplicateName
		}
		return Profile{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Profile{}, err
	}
	return Profile{ID: id, Name: name, Description: description, CreatedBy: createdBy, CreatedAt: now, UpdatedAt: now}, nil
}

func (s *Store) Profile(id int64) (Profile, error) {
	p, err := scanProfile(s.sql.QueryRow(`SELECT `+profileCols+` FROM profiles WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Profile{}, ErrNotFound
	}
	return p, err
}

func (s *Store) ProfileByName(name string) (Profile, error) {
	p, err := scanProfile(s.sql.QueryRow(`SELECT `+profileCols+` FROM profiles WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return Profile{}, ErrNotFound
	}
	return p, err
}

// PublishedProfiles — профили, доступные пользователям (published=1).
func (s *Store) PublishedProfiles() ([]Profile, error) {
	rows, err := s.sql.Query(`SELECT ` + profileCols + ` FROM profiles WHERE published = 1 ORDER BY name ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Profile
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// AllProfiles — все профили (admin-лист).
func (s *Store) AllProfiles() ([]Profile, error) {
	return s.queryProfiles(`SELECT ` + profileCols + ` FROM profiles ORDER BY name ASC`)
}

// ProfilesByOwner — профили, созданные пользователем (user-лист, только свои).
func (s *Store) ProfilesByOwner(userID int64) ([]Profile, error) {
	return s.queryProfiles(`SELECT `+profileCols+` FROM profiles WHERE created_by = ? ORDER BY name ASC`, userID)
}

func (s *Store) queryProfiles(q string, args ...any) ([]Profile, error) {
	rows, err := s.sql.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Profile
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// OwnedProfile — профиль, если он принадлежит userID (иначе ErrNotFound:
// чужой id не подтверждаем существованием).
func (s *Store) OwnedProfile(id, userID int64) (Profile, error) {
	p, err := s.Profile(id)
	if err != nil {
		return Profile{}, err
	}
	if p.CreatedBy != userID {
		return Profile{}, ErrNotFound
	}
	return p, nil
}

// DeleteProfile удаляет профиль и все его версии (каскад вручную — SQLite
// foreign_keys=ON без ON DELETE CASCADE). Нет профиля → ErrNotFound.
func (s *Store) DeleteProfile(id int64) error {
	tx, err := s.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// H4: a pinned non-terminal run or an active management policy keeps the
	// profile (and its versions) alive. Terminal runs may still reference the
	// historical version after deletion (read returns 404 by design).
	var inUse int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM profile_runs r JOIN profile_versions pv ON pv.id = r.profile_version_id
		 WHERE pv.profile_id = ? AND r.state IN ('pending','running','waiting_retry','waiting_event','reconciling','needs_attention','paused')`,
		id).Scan(&inUse); err != nil {
		return err
	}
	if inUse > 0 {
		return ErrProfileInUse
	}
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM node_management m JOIN profile_versions pv ON pv.id = m.profile_version_id
		 WHERE pv.profile_id = ?`, id).Scan(&inUse); err != nil {
		return err
	}
	if inUse > 0 {
		return ErrProfileInUse
	}
	// Версии удаляем первыми: FK profile_versions.profile_id → profiles(id).
	if _, err := tx.Exec(`DELETE FROM profile_versions WHERE profile_id = ?`, id); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM profiles WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// SetProfilePublished переводит published (требует версии, если true —
// PublishProfile это гарантирует).
func (s *Store) SetProfilePublished(id int64, published bool) (Profile, error) {
	if published {
		return s.PublishProfile(id)
	}
	if _, err := s.Profile(id); err != nil {
		return Profile{}, err
	}
	if _, err := s.sql.Exec(`UPDATE profiles SET published = 0, updated_at = ? WHERE id = ?`, time.Now().Unix(), id); err != nil {
		return Profile{}, err
	}
	return s.Profile(id)
}

// UpdateProfileDescription правит метаданные, не трогая версии.
func (s *Store) UpdateProfileDescription(id int64, description string) (Profile, error) {
	if _, err := s.sql.Exec(`UPDATE profiles SET description = ?, updated_at = ? WHERE id = ?`, description, time.Now().Unix(), id); err != nil {
		return Profile{}, err
	}
	return s.Profile(id)
}

// RenameProfile переименовывает профиль. Это ПРАВКА МЕТАДАННЫХ, а не версии:
// в отличие от steps/vars_schema (append-only, каждая правка = новая версия),
// переименование НЕ создаёт новую версию и не сбрасывает published — имя лишь
// подпись профиля. Имя тримится, требуется непустым и уникальным без учёта
// регистра (колонка profiles.name в SQLite BINARY — регистр она не ловит,
// поэтому сравниваем сами, Unicode-aware). Совпадение с текущим именем —
// no-op (updated_at не трогаем).
func (s *Store) RenameProfile(id int64, name string) (Profile, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Profile{}, fmt.Errorf("%w: profile name required", ErrValidation)
	}
	cur, err := s.Profile(id)
	if err != nil {
		return Profile{}, err
	}
	if cur.Name == name {
		return cur, nil // уже так называется — ничего не пишем
	}
	all, err := s.AllProfiles()
	if err != nil {
		return Profile{}, err
	}
	for _, p := range all {
		if p.ID != id && strings.EqualFold(p.Name, name) {
			return Profile{}, ErrDuplicateName
		}
	}
	if _, err := s.sql.Exec(`UPDATE profiles SET name = ?, updated_at = ? WHERE id = ?`, name, time.Now().Unix(), id); err != nil {
		if isUniqueViolation(err) { // гонка с параллельным переименованием
			return Profile{}, ErrDuplicateName
		}
		return Profile{}, err
	}
	return s.Profile(id)
}

// CountUserProfiles — число профилей владельца (лимит max_user_profiles).
func (s *Store) CountUserProfiles(userID int64) (int, error) {
	var n int
	err := s.sql.QueryRow(`SELECT COUNT(*) FROM profiles WHERE created_by = ?`, userID).Scan(&n)
	return n, err
}

// CountProfileVersions — число версий профиля (admin JSON §7.5).
func (s *Store) CountProfileVersions(profileID int64) (int, error) {
	var n int
	err := s.sql.QueryRow(`SELECT COUNT(*) FROM profile_versions WHERE profile_id = ?`, profileID).Scan(&n)
	return n, err
}

// DefaultProfileName — штатный профиль установки ноды (addendum §4.4).
const DefaultProfileName = "sharedd-node"

// DefaultProfileSteps — штатный профиль: MTProxyL с бинарным движком
// (mtproxyl-bin) + агент sharedd. Канал агента выбирается переменной
// sharedd_channel (stable|dev). port/sni/secret/ad_tag — переменные установки;
// label, node_secret, registry_url инъектируются сервером. Синтаксис
// `{{ var }}` shell-квотится ровно один раз.
const DefaultProfileSteps = `# Установка MTProxyL (manager, binary engine)
curl -fsSL https://raw.githubusercontent.com/Liafanx/MTProxyL/main/install.sh -o /root/mtproxyl-install.sh
ENGINE_URL={{ engine_url }}
TAG={{ ad_tag }}
EXTRA=""
[ -n "$ENGINE_URL" ] && EXTRA="--engine-url $ENGINE_URL"
[ -n "$TAG" ] && EXTRA="$EXTRA --ad-tag $TAG"
bash /root/mtproxyl-install.sh -- --mode manager --engine binary $EXTRA --proxy-mode mtproto --port {{ port }} --sni {{ sni }} --secret {{ secret }} --mask on --sni-policy reject_handshake --force

# Установка агента sharedd выбранного канала (stable|dev) и запуск.
# --name={{ label }}: короткое имя, суффикс -<5hex> добавит агент;
# персональный node_secret — Bearer machine-API.
case {{ sharedd_channel }} in
  stable|dev) ;;
  *) echo 'sharedd_channel must be stable or dev' >&2; exit 1 ;;
esac
curl -fsSL https://raw.githubusercontent.com/ugDeDe/sharedd/main/scripts/install_node_web.sh | sudo bash -s -- --channel={{ sharedd_channel }} --registry={{ registry_url }} --name={{ label }} --registry-token={{ node_secret }}

# Проверка: агент поднялся И реально зарегистрировался в реестре (DEF-B:
# провал /register (неверный секрет, 403-карантин, неверный порт) делает
# установку failed, а не ложный done). node_id реестру до claim неизвестен —
# статус резолвится реестром по самому Bearer'у. Ограниченные повторы: агент
# стартует асинхронно после systemctl.
systemctl is-active --quiet sharedd-node-agent
TOKEN={{ node_secret }}
URL={{ registry_url }}/api/node/status
for i in {1..30}; do
  if curl -fsS --max-time 5 -H "Authorization: Bearer $TOKEN" "$URL" >/dev/null 2>&1; then
    exit 0
  fi
  sleep 2
done
echo 'node did not register with the registry' >&2
exit 1
`

// DefaultProfileVarsSchema — переменные штатного профиля. engine_url и ad_tag
// необязательны (пусто = выбор MTProxyL); sharedd_channel — канал агента.
const DefaultProfileVarsSchema = `{"vars":[{"name":"engine_url","description":"URL архива telemt (пусто = выбор MTProxyL)","default":""},{"name":"ad_tag","description":"Рекламный тег MTProxyL","default":""},{"name":"sharedd_channel","description":"Канал агента sharedd: stable или dev","default":"stable"},{"name":"api_addr","description":"Адрес telemt API"},{"name":"port","description":"Порт shared_proxy","default":"443"}]}`

// legacyDefaultMarker — признак старой версии штатного профиля (telemt
// через MTPROTO_FIX). Такие версии автоматически дополняются новой.
const legacyDefaultMarker = "MTPROTO_FIX_By_MEKO"

// DefaultProfileDescription — подпись штатного профиля.
const DefaultProfileDescription = "Штатная установка ноды sharedd (MTProxyL + агент)"

// legacyDefaultDescription — прежняя подпись (telemt); меняется на актуальную.
const legacyDefaultDescription = "Штатная установка ноды sharedd (telemt + агент)"

// EnsureDefaultProfile создаёт штатный профиль (опубликованный) при первом
// запуске, если его ещё нет. Идемпотентно: существующий не трогаем, кроме
// легаси-содержимого (telemt/MTPROTO_FIX): оно перезаписывается in place
// (версий больше нет — у профиля одна строка контента).
func (s *Store) EnsureDefaultProfile() (Profile, error) {
	if p, err := s.ProfileByName(DefaultProfileName); err == nil {
		v, verr := s.ProfileVersion(p.ID, 0)
		if verr != nil && !errors.Is(verr, ErrNotFound) {
			return Profile{}, verr
		}
		if verr != nil || strings.Contains(v.Steps, legacyDefaultMarker) {
			if _, nerr := s.AddProfileVersion(p.ID, DefaultProfileSteps, DefaultProfileVarsSchema); nerr != nil {
				return Profile{}, nerr
			}
			if p.Description == legacyDefaultDescription {
				_, _ = s.UpdateProfileDescription(p.ID, DefaultProfileDescription)
			}
			_, _ = s.SetProfilePublished(p.ID, true)
			return s.Profile(p.ID)
		}
		return p, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Profile{}, err
	}
	p, err := s.CreateProfile(DefaultProfileName, DefaultProfileDescription, 0)
	if err != nil {
		if errors.Is(err, ErrDuplicateName) { // гонка двух стартов — берём победителя
			return s.ProfileByName(DefaultProfileName)
		}
		return Profile{}, err
	}
	if _, err := s.AddProfileVersion(p.ID, DefaultProfileSteps, DefaultProfileVarsSchema); err != nil {
		return Profile{}, err
	}
	return s.PublishProfile(p.ID)
}

// AddProfileVersion записывает контент профиля. Версий больше нет: если у
// профиля есть строка контента — она обновляется IN PLACE (номер не меняется),
// иначе создаётся первая строка. Published не сбрасывается: правка — это та же
// единственная версия профиля.
func (s *Store) AddProfileVersion(profileID int64, steps, varsSchema string) (ProfileVersion, error) {
	if steps == "" {
		return ProfileVersion{}, fmt.Errorf("%w: steps required", ErrValidation)
	}
	if err := ValidateProfileVarsSchema(varsSchema); err != nil {
		return ProfileVersion{}, err
	}
	if _, err := s.Profile(profileID); err != nil {
		return ProfileVersion{}, err
	}
	latest, err := s.ProfileVersion(profileID, 0)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return ProfileVersion{}, err
	}
	now := time.Now().Unix()
	if err == nil {
		// Колонка БД исторически называется playbook — см. комментарий к пакету.
		// Переход graph→text очищает blueprint-поля этой же строки.
		if _, err := s.sql.Exec(`UPDATE profile_versions
			SET playbook = ?, vars_schema = ?, format = ?, blueprint_json = '', schema_version = 0, content_hash = ''
			WHERE id = ?`, steps, varsSchema, FormatText, latest.ID); err != nil {
			return ProfileVersion{}, err
		}
		return ProfileVersion{ID: latest.ID, ProfileID: profileID, Version: latest.Version,
			Steps: steps, VarsSchema: varsSchema, Format: FormatText, CreatedAt: latest.CreatedAt}, nil
	}
	res, err := s.sql.Exec(`INSERT INTO profile_versions (profile_id, version, playbook, vars_schema, created_at, format)
		VALUES (?,?,?,?,?,?)`, profileID, 1, steps, varsSchema, now, FormatText)
	if err != nil {
		return ProfileVersion{}, err
	}
	id, _ := res.LastInsertId()
	return ProfileVersion{ID: id, ProfileID: profileID, Version: 1, Steps: steps, VarsSchema: varsSchema, Format: FormatText, CreatedAt: now}, nil
}

// PublishProfile — делает профиль доступным пользователям. Требует ≥1 версии.
func (s *Store) PublishProfile(id int64) (Profile, error) {
	if _, err := s.ProfileVersion(id, 0); err != nil {
		return Profile{}, err
	}
	if _, err := s.sql.Exec(`UPDATE profiles SET published = 1, updated_at = ? WHERE id = ?`, time.Now().Unix(), id); err != nil {
		return Profile{}, err
	}
	return s.Profile(id)
}

// ProfileVersion — версия профиля; version <= 0 → последняя.
func (s *Store) ProfileVersion(profileID int64, version int) (ProfileVersion, error) {
	q := `SELECT ` + profileVersionCols + ` FROM profile_versions WHERE profile_id = ?`
	args := []any{profileID}
	if version <= 0 {
		q += ` ORDER BY version DESC LIMIT 1`
	} else {
		q += ` AND version = ?`
		args = append(args, version)
	}
	var v ProfileVersion
	err := s.sql.QueryRow(q, args...).Scan(&v.ID, &v.ProfileID, &v.Version, &v.Steps, &v.VarsSchema, &v.CreatedAt,
		&v.Format, &v.Blueprint, &v.SchemaVersion, &v.ContentHash)
	if errors.Is(err, sql.ErrNoRows) {
		return ProfileVersion{}, ErrNotFound
	}
	return v, err
}

// ProfileVersionByID / AuthoredVersionFor return an immutable version by its
// numeric version (never latest); version <= 0 is ErrNotFound. Both names are
// the same lookup for the stage-08a versions route.
func (s *Store) ProfileVersionByID(profileID int64, version int) (ProfileVersion, error) {
	if version <= 0 {
		return ProfileVersion{}, ErrNotFound
	}
	return s.ProfileVersion(profileID, version)
}

func (s *Store) AuthoredVersionFor(profileID int64, version int) (ProfileVersion, error) {
	return s.ProfileVersionByID(profileID, version)
}

// CreateProfileVersionGraph записывает граф-контент профиля in place: если
// строка контента уже есть, она обновляется (номер не меняется), иначе
// создаётся первая. Текст и vars_schema очищаются при переходе text→graph.
func (s *Store) CreateProfileVersionGraph(profileID int64, blueprintJSON string, schemaVersion int, contentHash string) (ProfileVersion, error) {
	if strings.TrimSpace(blueprintJSON) == "" || schemaVersion <= 0 || contentHash == "" {
		return ProfileVersion{}, fmt.Errorf("%w: blueprint, schema_version and content_hash required", ErrValidation)
	}
	if _, err := s.Profile(profileID); err != nil {
		return ProfileVersion{}, err
	}
	latest, err := s.ProfileVersion(profileID, 0)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return ProfileVersion{}, err
	}
	now := time.Now().Unix()
	if err == nil {
		if _, err := s.sql.Exec(`UPDATE profile_versions
			SET playbook = '', vars_schema = '', format = ?, blueprint_json = ?, schema_version = ?, content_hash = ?
			WHERE id = ?`, FormatGraph, blueprintJSON, schemaVersion, contentHash, latest.ID); err != nil {
			return ProfileVersion{}, err
		}
		return ProfileVersion{ID: latest.ID, ProfileID: profileID, Version: latest.Version,
			Format: FormatGraph, Blueprint: blueprintJSON, SchemaVersion: schemaVersion,
			ContentHash: contentHash, CreatedAt: latest.CreatedAt}, nil
	}
	res, err := s.sql.Exec(`INSERT INTO profile_versions
		(profile_id, version, playbook, vars_schema, created_at, format, blueprint_json, schema_version, content_hash)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		profileID, 1, "", "", now, FormatGraph, blueprintJSON, schemaVersion, contentHash)
	if err != nil {
		return ProfileVersion{}, err
	}
	id, _ := res.LastInsertId()
	return ProfileVersion{
		ID: id, ProfileID: profileID, Version: 1, Format: FormatGraph,
		Blueprint: blueprintJSON, SchemaVersion: schemaVersion, ContentHash: contentHash, CreatedAt: now,
	}, nil
}

// LatestPublishedVersion — текущая опубликованная версия профиля.
func (s *Store) LatestPublishedVersion(profileID int64) (ProfileVersion, error) {
	p, err := s.Profile(profileID)
	if err != nil {
		return ProfileVersion{}, err
	}
	if !p.Published {
		return ProfileVersion{}, ErrNotFound
	}
	return s.ProfileVersion(profileID, 0)
}

// EnsureSystemGraphProfile seeds an embedded system graph profile
// (created_by=0, published=1, format=graph). Idempotent: an existing profile
// is returned untouched, never overwritten. The first version carries the
// caller-computed canonical content hash.
func (s *Store) EnsureSystemGraphProfile(name, description, blueprintJSON string, schemaVersion int, contentHash string) (Profile, error) {
	name = strings.TrimSpace(name)
	if name == "" || blueprintJSON == "" || schemaVersion <= 0 || contentHash == "" {
		return Profile{}, fmt.Errorf("%w: system graph fields required", ErrValidation)
	}
	if p, err := s.ProfileByName(name); err == nil {
		return p, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Profile{}, err
	}
	p, err := s.CreateProfile(name, description, 0)
	if err != nil {
		if errors.Is(err, ErrDuplicateName) { // startup race between replicas
			return s.ProfileByName(name)
		}
		return Profile{}, err
	}
	if _, err := s.CreateProfileVersionGraph(p.ID, blueprintJSON, schemaVersion, contentHash); err != nil {
		return Profile{}, err
	}
	return s.PublishProfile(p.ID)
}

// SystemGraphVersionID — numeric profile_versions.id of the latest version of
// a seeded system graph. Missing/unseeded profile → ErrNotFound.
func (s *Store) SystemGraphVersionID(name string) (int64, error) {
	p, err := s.ProfileByName(name)
	if err != nil {
		return 0, err
	}
	v, err := s.ProfileVersion(p.ID, 0)
	if err != nil {
		return 0, err
	}
	if v.Format != FormatGraph || v.ContentHash == "" {
		return 0, ErrValidation
	}
	return v.ID, nil
}

// ── Переменные профиля (addendum §6) ──────────────────────────────
//
// vars_schema хранит ТАБЛИЦУ объявленных переменных:
//
//	{"vars":[{"name":"testvar","default":"x","description":"...","secret":true}]}
//
// Пользователь может переопределить объявленные переменные И добавить свои
// сверх объявленных; поэтому проверяется не членство в схеме, а формат и
// размеры. Значения подставляются в шаги через text/template и КАЖДОЕ
// shell-квотится (control.RenderStep) — инъекция через значение невозможна.

const (
	// MaxExtraVars — общее число переменных extra-vars.
	MaxExtraVars = 64
	// MaxVarValueLen — длина одного значения.
	MaxVarValueLen = 4096
	// MaxExtraVarsBytes — суммарный размер extra-vars JSON.
	MaxExtraVarsBytes = 32 << 10
)

// ProfileVar — объявленная переменная профиля.
type ProfileVar struct {
	Name        string `json:"name"`
	Default     string `json:"default,omitempty"`
	Description string `json:"description,omitempty"`
	Secret      bool   `json:"secret,omitempty"`
}

var varNameRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// ParseProfileVars читает таблицу переменных из vars_schema. Пустая строка → nil.
func ParseProfileVars(varsSchema string) ([]ProfileVar, error) {
	if strings.TrimSpace(varsSchema) == "" {
		return nil, nil
	}
	if !json.Valid([]byte(varsSchema)) {
		return nil, fmt.Errorf("%w: vars_schema is not valid JSON", ErrValidation)
	}
	var env struct {
		Vars []ProfileVar `json:"vars"`
	}
	if err := json.Unmarshal([]byte(varsSchema), &env); err != nil {
		return nil, fmt.Errorf("%w: bad vars_schema", ErrValidation)
	}
	return env.Vars, nil
}

// ValidateProfileVarsSchema — проверка на СОХРАНЕНИИ профиля: схема валидна и
// имена соответствуют regex.
func ValidateProfileVarsSchema(varsSchema string) error {
	decls, err := ParseProfileVars(varsSchema)
	if err != nil {
		return err
	}
	for _, d := range decls {
		if err := validateEditableVarName(d.Name); err != nil {
			return err
		}
		// Empty declared defaults mean inheritance, not an installation override.
		if d.Default != "" {
			if err := validateProxyVar(d.Name, d.Default); err != nil {
				return err
			}
		}
	}
	return nil
}

// MergeProfileVars применяет дефолты объявленных переменных к пользовательским
// (пользователь главнее), затем валидирует результат перед запуском.
func MergeProfileVars(varsSchema string, userVars map[string]any) (map[string]any, error) {
	if err := ValidateProfileVarsSchema(varsSchema); err != nil {
		return nil, err
	}
	decls, err := ParseProfileVars(varsSchema)
	if err != nil {
		return nil, err
	}
	merged := make(map[string]any, len(userVars)+len(decls))
	for k, v := range userVars {
		if err := validateEditableVarName(k); err != nil {
			return nil, err
		}
		k = canonicalProxyVar(k)
		if existing, ok := merged[k]; ok && fmt.Sprint(existing) != fmt.Sprint(v) {
			return nil, fmt.Errorf("%w: conflicting aliases for %q", ErrValidation, k)
		}
		merged[k] = v
	}
	for _, d := range decls {
		name := canonicalProxyVar(d.Name)
		if _, ok := merged[name]; !ok && d.Default != "" {
			merged[name] = d.Default
		}
	}
	if v, ok := merged["ad_tag"].(string); ok {
		merged["ad_tag"] = strings.ToLower(strings.TrimSpace(v))
	}
	if err := ValidateExtraVars(merged); err != nil {
		return nil, err
	}
	return merged, nil
}

func validateEditableVarName(name string) error {
	if !varNameRe.MatchString(name) {
		return fmt.Errorf("%w: illegal variable name %q", ErrValidation, name)
	}
	switch name {
	case "node_id", "node_secret", "registry_url", "label":
		return fmt.Errorf("%w: variable %q is server controlled", ErrValidation, name)
	}
	return nil
}

func canonicalProxyVar(name string) string {
	switch name {
	case "port", "sni", "secret":
		return "proxy_" + name
	default:
		return name
	}
}

func validateProxyVar(name string, value any) error {
	name = canonicalProxyVar(name)
	switch name {
	case "proxy_port":
		text := fmt.Sprint(value)
		port, err := strconv.ParseUint(text, 10, 16)
		if err != nil || port == 0 || strings.Trim(text, "0123456789") != "" {
			return fmt.Errorf("%w: proxy_port must be numeric 1..65535", ErrValidation)
		}
	case "proxy_sni":
		sni, ok := value.(string)
		if !ok || !config.ValidHostname(sni) || net.ParseIP(sni) != nil {
			return fmt.Errorf("%w: proxy_sni must be a domain name without a path or port", ErrValidation)
		}
	case "ad_tag":
		// MTProto ad tag: 32 hex characters. Empty is an explicit "no tag"
		// and is allowed; absence is inheritance and handled by merge.
		tag, ok := value.(string)
		if !ok {
			return fmt.Errorf("%w: ad_tag must be a string", ErrValidation)
		}
		tag = strings.TrimSpace(tag)
		if tag == "" {
			return nil
		}
		raw, err := hex.DecodeString(tag)
		if err != nil || len(raw) != 16 || len(tag) != 32 {
			return fmt.Errorf("%w: ad_tag must be 32 hex characters", ErrValidation)
		}
	}
	return nil
}

// ValidateExtraVars — проверка ПЕРЕД запуском: имя (regex), длина значения
// ≤ 4096, число переменных ≤ 64, суммарный JSON ≤ 32 KiB.
func ValidateExtraVars(vars map[string]any) error {
	if len(vars) > MaxExtraVars {
		return fmt.Errorf("%w: too many variables (%d > %d)", ErrValidation, len(vars), MaxExtraVars)
	}
	for name, val := range vars {
		if !varNameRe.MatchString(name) {
			return fmt.Errorf("%w: illegal variable name %q", ErrValidation, name)
		}
		if err := validateProxyVar(name, val); err != nil {
			return err
		}
		if n := len(fmt.Sprint(val)); n > MaxVarValueLen {
			return fmt.Errorf("%w: variable %q value exceeds %d bytes", ErrValidation, name, MaxVarValueLen)
		}
	}
	if len(vars) > 0 {
		blob, err := json.Marshal(vars)
		if err != nil {
			return fmt.Errorf("%w: variables are not JSON-serialisable", ErrValidation)
		}
		if len(blob) > MaxExtraVarsBytes {
			return fmt.Errorf("%w: extra-vars exceed %d bytes", ErrValidation, MaxExtraVarsBytes)
		}
	}
	return nil
}

// SecretVarNames — имена объявленных secret-переменных (для маскирования
// значений в выводе operations).
func SecretVarNames(varsSchema string) []string {
	decls, err := ParseProfileVars(varsSchema)
	if err != nil {
		return nil
	}
	var out []string
	for _, d := range decls {
		if d.Secret {
			out = append(out, d.Name)
		}
	}
	return out
}

// ProfileVersionByNumericID returns an immutable version by its surrogate PK
// (profile_versions.id), which graph run inputs reference as
// installation_profile_version_id. Numeric *version* lookups stay in
// ProfileVersionByID.
func (s *Store) ProfileVersionByNumericID(id int64) (ProfileVersion, error) {
	if id <= 0 {
		return ProfileVersion{}, ErrNotFound
	}
	var v ProfileVersion
	err := s.sql.QueryRow(`SELECT `+profileVersionCols+` FROM profile_versions WHERE id = ?`, id).
		Scan(&v.ID, &v.ProfileID, &v.Version, &v.Steps, &v.VarsSchema, &v.CreatedAt,
			&v.Format, &v.Blueprint, &v.SchemaVersion, &v.ContentHash)
	if errors.Is(err, sql.ErrNoRows) {
		return ProfileVersion{}, ErrNotFound
	}
	return v, err
}
