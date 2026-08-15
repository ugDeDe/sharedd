package main

// Персистентная история: SQLite рядом с JSON-стейтом.
//
// Зачем так: горячее членство пула (кандидаты, heartbeat-счётчики, серии)
// меняется десятки раз в секунду — держать его в БД неоптимально, оно по-
// прежнему живёт в памяти + registry_state.json. А вот ФАКТЫ (события и
// терминальные блокировки нод) — редкие и должны переживать всё: их и
// кладём в SQLite (embedded, чистый Go — modernc.org/sqlite, CGO не нужен).
//
// Две таблицы:
//
//	bans — терминальные блокировки нод (без восстановления). НИКОГДА не
//	 ротируются: из неё публичный /dashboard считает три метрики —
//	 число банов GP, периодичность банов (интервал между блокировками)
//	 и среднее время жизни ноды (регистрация → бан). gap_sec —
//	 интервал от предыдущего бана ЛЮБОГО типа, считается на вставке
//	 (храним, чтобы периодичность не зависела от вычислений при чтении).
//	 Пишутся только баны нод, успевших поработать мастером
//	 (terminate.go hadMasterTime), и ОДИН РАЗ на ip: повторные баны
//	 адреса строку не добавляют, а восстановившийся адрес (reverify
//	 прошёл) свою строку из статистики убирает (liftBanIP).
//	events — зеркало журнала событий (addEventLocked). Ротируется по
//	 [database] events_retention_days (умолчание 30 суток).
//
// БД — необязательный компонент: файл не открылся/сломан → регистратор
// продолжает работать как раньше (r.db == nil), просто без длинной истории.
// Записи синхронные и дешёвые (events/баны — единицы в секунду в пике флапа);
// sql.DB сам потокобезопасен, отдельный лок не нужен.
//
// ВАЖНО: вызовы record* происходят из точек, где уже держится r.mu — SQLite
// на WAL это переживает (sub-mс на записи отлично укладываются рядом с
// persistStateLocked, который под тем же локом пишет JSON целиком), но
// НИКАКИХ сетевых вызовов сюда добавлять нельзя.

import (
	"database/sql"
	"log"
	"time"

	_ "modernc.org/sqlite"
)

// ban reasons (см. terminate.go — там же тексты сообщений агенту).
const (
	BanReasonIPBan = "ip_ban" // блокировка по IP (финал GP-карантина)
	BanReasonDead  = "dead"   // регистратор не достучался до порта/метрик >N мин
)

// banRow — одна терминальная блокировка ноды.
type banRow struct {
	TS          time.Time `json:"ts"`
	NodeID      string    `json:"node_id"`
	IP          string    `json:"ip"`
	Reason      string    `json:"reason"`       // ip_ban | dead
	LifetimeSec int64     `json:"lifetime_sec"` // регистрация → бан; -1 = неизвестно (retire после expiry)
	GapSec      int64     `json:"gap_sec"`      // интервал от предыдущего бана любого типа; -1 = первый
}

type historyDB struct {
	sql *sql.DB
}

// openHistoryDB — открыть (создать) файл истории и гарантировать схему.
// Ошибка → nil БД (регистратор работает без истории), причина в логе.
func openHistoryDB(path string) *historyDB {
	if path == "" {
		return nil
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		log.Printf("history db %s: open failed: %v — continuing WITHOUT persistent history", path, err)
		return nil
	}
	// одно соединение: иначе in-memory/тестовые файлы получают «table not found»
	// между соединениями пула, а на проде — лишние busy-гонки на WAL.
	db.SetMaxOpenConns(1)
	const schema = `
CREATE TABLE IF NOT EXISTS bans (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 ts INTEGER NOT NULL,
 node_id TEXT NOT NULL,
 ip TEXT NOT NULL,
 reason TEXT NOT NULL,
 lifetime_sec INTEGER NOT NULL DEFAULT -1,
 gap_sec INTEGER NOT NULL DEFAULT -1
);
CREATE INDEX IF NOT EXISTS idx_bans_ts ON bans(ts);
CREATE TABLE IF NOT EXISTS events (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 ts INTEGER NOT NULL,
 type TEXT NOT NULL,
 node_id TEXT,
 ip TEXT,
 domain TEXT,
 detail TEXT
);
CREATE INDEX IF NOT EXISTS idx_events_ts ON events(ts);`
	if _, err := db.Exec(schema); err != nil {
		log.Printf("history db %s: schema failed: %v — continuing WITHOUT persistent history", path, err)
		db.Close()
		return nil
	}
	log.Printf("history db: %s (sqlite, WAL)", path)
	return &historyDB{sql: db}
}

func (d *historyDB) Close() { d.sql.Close() }

// recordBan — терминальная блокировка ноды. ОДИН IP пишется как бан только
// ОДИН РАЗ: строка по адресу уже есть (ноду перепроверили и забанили снова,
// она успела побывать мастером и т.п.) — новая не добавляется, повторные
// баны адреса статистику не накручивают. Запись перестаёт существовать,
// когда адрес восстанавливается (liftBanIP) — после этого его новый бан
// снова пишется как первый. gap считается от предыдущего бана любого типа
// (периодичность «между событиями блокировки любых нод»).
func (d *historyDB) recordBan(b banRow) {
	if b.IP != "" {
		var exists int
		if err := d.sql.QueryRow(`SELECT COUNT(*) FROM bans WHERE ip = ?`, b.IP).Scan(&exists); err == nil && exists > 0 {
			log.Printf("history db: ip %s already banned (%s, %s) — duplicate row skipped", b.IP, b.NodeID, b.Reason)
			return
		}
	}
	gap := int64(-1)
	var prevTS int64
	if err := d.sql.QueryRow(`SELECT ts FROM bans ORDER BY ts DESC, id DESC LIMIT 1`).Scan(&prevTS); err == nil {
		gap = b.TS.Unix() - prevTS
	}
	if _, err := d.sql.Exec(
		`INSERT INTO bans (ts, node_id, ip, reason, lifetime_sec, gap_sec) VALUES (?,?,?,?,?,?)`,
		b.TS.Unix(), b.NodeID, b.IP, b.Reason, b.LifetimeSec, gap); err != nil {
		log.Printf("history db: record ban %s (%s): %v", b.NodeID, b.Reason, err)
	}
}

// liftBanIP — адрес ВОССТАНОВИЛСЯ (например, gp re-verify после бана прошёл):
// запись о его бане убирается из статистики. Возвращает число удалённых строк.
func (d *historyDB) liftBanIP(ip string) int {
	if ip == "" {
		return 0
	}
	res, err := d.sql.Exec(`DELETE FROM bans WHERE ip = ?`, ip)
	if err != nil {
		log.Printf("history db: lift ban of %s: %v", ip, err)
		return 0
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		log.Printf("history db: ip %s recovered — %d ban row(s) removed from stats", ip, n)
	}
	return int(n)
}

// recordEvent — зеркало события журнала (вызывается из addEventLocked).
func (d *historyDB) recordEvent(ev Event) {
	if _, err := d.sql.Exec(
		`INSERT INTO events (ts, type, node_id, ip, domain, detail) VALUES (?,?,?,?,?,?)`,
		ev.At.Unix(), ev.Type, ev.NodeID, ev.IP, ev.Domain, ev.Detail); err != nil {
		log.Printf("history db: record event %s: %v", ev.Type, err)
	}
}

// pruneEvents — ротация журнала в БД. bans НЕ трогаем никогда: три метрики
// дашборда (баны/периодичность/время жизни) должны жить постоянно.
func (d *historyDB) pruneEvents(olderThan time.Time) {
	res, err := d.sql.Exec(`DELETE FROM events WHERE ts < ?`, olderThan.Unix())
	if err != nil {
		log.Printf("history db: events retention: %v", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("history db: events retention removed %d rows (older than %s)", n, olderThan.Format("2006-01-02"))
	}
}

// bansSince — блокировки новее since, по возрастанию времени. reasonFilter:
// "" — все, иначе только этого типа.
func (d *historyDB) bansSince(since time.Time, reasonFilter string) ([]banRow, error) {
	q := `SELECT ts, node_id, ip, reason, lifetime_sec, gap_sec FROM bans WHERE ts >= ?`
	args := []any{since.Unix()}
	if reasonFilter != "" {
		q += ` AND reason = ?`
		args = append(args, reasonFilter)
	}
	q += ` ORDER BY ts ASC, id ASC`
	rows, err := d.sql.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []banRow{}
	for rows.Next() {
		var b banRow
		var ts int64
		if err := rows.Scan(&ts, &b.NodeID, &b.IP, &b.Reason, &b.LifetimeSec, &b.GapSec); err != nil {
			return nil, err
		}
		b.TS = time.Unix(ts, 0)
		out = append(out, b)
	}
	return out, rows.Err()
}

// historyDBLoop — ротация событий раз в сутки (+ прогон на старте).
func (r *Registry) historyDBLoop() {
	if r.db == nil {
		return
	}
	r.db.pruneEvents(time.Now().Add(-r.cfg.EventsRetention))
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		r.db.pruneEvents(time.Now().Add(-r.cfg.EventsRetention))
	}
}
