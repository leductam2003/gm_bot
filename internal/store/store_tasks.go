package store

import (
	"database/sql"
	"errors"
	"time"
)

// TaskRow is the persisted shape of a task: a JSON config blob plus indexable
// columns. Per-wallet runtime status is ephemeral and lives in the engine.
type TaskRow struct {
	ID         int64     `json:"id"`
	GroupName  string    `json:"group"`
	ConfigJSON string    `json:"-"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"createdAt"`
}

func (s *Store) migrateTasks() error {
	const schema = `
CREATE TABLE IF NOT EXISTS tasks (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  group_name  TEXT NOT NULL DEFAULT 'Imported',
  config_json TEXT NOT NULL,
  status      TEXT NOT NULL DEFAULT 'idle',
  created_at  INTEGER NOT NULL
);`
	_, err := s.db.Exec(schema)
	return err
}

func (s *Store) AddTask(group, configJSON string) (int64, error) {
	if group == "" {
		group = "Imported"
	}
	res, err := s.db.Exec(
		`INSERT INTO tasks(group_name,config_json,status,created_at) VALUES(?,?,?,?)`,
		group, configJSON, "idle", time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) ListTasks() ([]TaskRow, error) {
	rows, err := s.db.Query(`SELECT id,group_name,config_json,status,created_at FROM tasks ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskRow
	for rows.Next() {
		var t TaskRow
		var ts int64
		if err := rows.Scan(&t.ID, &t.GroupName, &t.ConfigJSON, &t.Status, &ts); err != nil {
			return nil, err
		}
		t.CreatedAt = time.Unix(ts, 0)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) GetTask(id int64) (TaskRow, error) {
	var t TaskRow
	var ts int64
	err := s.db.QueryRow(
		`SELECT id,group_name,config_json,status,created_at FROM tasks WHERE id=?`, id).
		Scan(&t.ID, &t.GroupName, &t.ConfigJSON, &t.Status, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	t.CreatedAt = time.Unix(ts, 0)
	return t, err
}

func (s *Store) UpdateTaskConfig(id int64, group, configJSON string) error {
	_, err := s.db.Exec(`UPDATE tasks SET group_name=?, config_json=? WHERE id=?`, group, configJSON, id)
	return err
}

func (s *Store) UpdateTaskStatus(id int64, status string) error {
	_, err := s.db.Exec(`UPDATE tasks SET status=? WHERE id=?`, status, id)
	return err
}

func (s *Store) DeleteTask(id int64) error {
	_, err := s.db.Exec(`DELETE FROM tasks WHERE id=?`, id)
	return err
}
