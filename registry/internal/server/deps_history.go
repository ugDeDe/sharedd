package server

import (
	"time"

	"sharedd/registry/internal/history"
)

// Алиасы истории: сервер владеет циклом ротации, пакет history — схемой и
// запросами SQLite. Методы DB экспортированы (кросс-пакетный вызов).
type (
	historyDB = history.DB
	banRow    = history.BanRow
)

var openHistoryDB = history.Open

// historyDBLoop — ротация событий раз в сутки (+ прогон на старте).
func (r *Registry) historyDBLoop() {
	if r.db == nil {
		return
	}
	r.db.PruneEvents(time.Now().Add(-r.cfg.EventsRetention))
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		r.db.PruneEvents(time.Now().Add(-r.cfg.EventsRetention))
	}
}
