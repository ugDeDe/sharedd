package control

// Пошаговый журнал установки профиля (docs/specs/plan-ui-batch.md Фаза 3).
// Одна строка на шаг профиля: имя шага, статус running/ok/error, длительность
// (started_at/finished_at в мс) и ХВОСТ вывода шага (секреты замаскированы
// вызывающим так же, как финальный last_error). Журнал только для чтения в
// кабинете: экран операции отдаёт шаги владельцу.

import "time"

// Статусы шага.
const (
	StepRunning = "running"
	StepOK      = "ok"
	StepError   = "error"
)

// OperationStep — одна строка журнала. Timestamps в миллисекундах Unix.
type OperationStep struct {
	ID          int64
	OperationID int64
	Idx         int
	Name        string
	Status      string
	StartedAt   int64
	FinishedAt  int64
	Output      string
}

// DurationMs — длительность шага; 0, пока шаг не завершён.
func (s OperationStep) DurationMs() int64 {
	if s.FinishedAt <= s.StartedAt {
		return 0
	}
	return s.FinishedAt - s.StartedAt
}

// ClearOperationSteps удаляет прошлые шаги операции: retry-install начинает
// журнал заново, чтобы шаг с тем же idx не дублировался в выдаче.
func (s *Store) ClearOperationSteps(operationID int64) error {
	_, err := s.sql.Exec(`DELETE FROM operation_steps WHERE operation_id = ?`, operationID)
	return err
}

// BeginOperationStep вставляет шаг в статусе running и возвращает его id.
func (s *Store) BeginOperationStep(operationID int64, idx int, name string) (int64, error) {
	res, err := s.sql.Exec(
		`INSERT INTO operation_steps (operation_id, idx, name, status, started_at, finished_at, output) VALUES (?,?,?,?,?,0,'')`,
		operationID, idx, name, StepRunning, time.Now().UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishOperationStep проставляет финальный статус, вывод и время завершения.
func (s *Store) FinishOperationStep(id int64, status, output string) error {
	_, err := s.sql.Exec(
		`UPDATE operation_steps SET status = ?, output = ?, finished_at = ? WHERE id = ?`,
		status, output, time.Now().UnixMilli(), id)
	return err
}

// OperationSteps — шаги операции по возрастанию idx.
func (s *Store) OperationSteps(operationID int64) ([]OperationStep, error) {
	rows, err := s.sql.Query(
		`SELECT id, operation_id, idx, name, status, started_at, finished_at, output FROM operation_steps WHERE operation_id = ? ORDER BY idx ASC, id ASC`,
		operationID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []OperationStep{}
	for rows.Next() {
		var st OperationStep
		if err := rows.Scan(&st.ID, &st.OperationID, &st.Idx, &st.Name, &st.Status, &st.StartedAt, &st.FinishedAt, &st.Output); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}
