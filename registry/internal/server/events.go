package server

import "time"

// Журнал событий регистратора: история блокировок, смен мастера, жизненный
// цикл нод. Хранится кольцевым буфером внутри State (персистится вместе с
// состоянием на диск), лимит — cfg.Panel.EventsMax.

// addEventLocked добавляет событие в журнал и помечает состояние грязным
// (сброс на диск делает persistStateLocked или eventPersistLoop).
// ВЫЗЫВАТЬ СТРОГО под r.mu (write-lock).
func (r *Registry) addEventLocked(ev Event) {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	r.state.Events = append(r.state.Events, ev)
	r.cfgMu.RLock()
	max := r.cfg.Panel.EventsMax
	r.cfgMu.RUnlock()
	if max > 0 && len(r.state.Events) > max {
		trimmed := make([]Event, max)
		copy(trimmed, r.state.Events[len(r.state.Events)-max:])
		r.state.Events = trimmed
	}
	r.eventsDirty = true
	// Вечная история — зеркалим событие в SQLite (см. db.go).
	// Запись sub-мс и под r.mu укладывается рядом с persistStateLocked.
	if r.db != nil {
		r.db.recordEvent(ev)
	}
}

// eventPersistLoop периодически сбрасывает журнал на диск, чтобы события
// (flap'и, блокировки) переживали рестарт/краш без немедленной записи на
// каждое событие. Критичные точки (мастер, регистрация) персистятся сразу
// через persistStateLocked в местах изменения.
func (r *Registry) eventPersistLoop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		r.mu.Lock()
		if r.eventsDirty {
			r.persistStateLocked()
		}
		r.mu.Unlock()
	}
}

// closeMasterStintLocked фиксирует время ноды в роли мастера (вызывается при
// потере мастерства/удалении ноды). Под write-lock.
func closeMasterStintLocked(c *Candidate, now time.Time) {
	if c.MasterSince.IsZero() {
		return
	}
	c.MasterSeconds += int64(now.Sub(c.MasterSince).Seconds())
	c.MasterSince = time.Time{}
}
