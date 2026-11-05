package storage

import (
	"database/sql"
	"time"
)

// --- Hotspot (method-level sampling) ---

// Hotspot storage modes mirror leak detection: dense (every interval, kept 1h) and
// hourly (aggregated at the top of each hour, kept 7 days).
const (
	HotspotModeDense  = "dense"
	HotspotModeHourly = "hourly"
)

// HotspotSetting is the per-process hotspot switch plus the mode the daemon resolved
type HotspotSetting struct {
	PID       int    `json:"pid"`
	Enabled   bool   `json:"enabled"`
	Mode      string `json:"mode"`   // "" (pending), "jfr", "jstack"
	Detail    string `json:"detail"` // human readable explanation of the resolved mode
	UpdatedAt string `json:"updatedAt"`
}

// HotspotStackRow is one folded stack to persist
type HotspotStackRow struct {
	Kind   string
	Stack  string
	Count  int64
	Weight int64
}

// HotspotStackPoint is a folded stack at a point in time
type HotspotStackPoint struct {
	Timestamp string `json:"timestamp"`
	Stack     string `json:"stack"`
	Count     int64  `json:"count"`
	Weight    int64  `json:"weight"`
}

func (db *DB) migrateHotspot() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS hotspot_settings (
			pid INTEGER PRIMARY KEY,
			enabled INTEGER NOT NULL DEFAULT 0,
			mode TEXT NOT NULL DEFAULT '',
			detail TEXT NOT NULL DEFAULT '',
			updated_at DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS hotspot_stacks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			pid INTEGER NOT NULL,
			mode TEXT NOT NULL,
			kind TEXT NOT NULL,
			stack TEXT NOT NULL,
			count INTEGER NOT NULL,
			weight INTEGER NOT NULL,
			created_at DATETIME NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_hotspot_pid_mode_kind_ts ON hotspot_stacks(pid, mode, kind, created_at)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return err
		}
	}
	return nil
}

// GetHotspotSetting returns the setting for pid (disabled when none stored)
func (db *DB) GetHotspotSetting(pid int) (*HotspotSetting, error) {
	s := &HotspotSetting{PID: pid}
	var enabled int
	err := db.QueryRow(
		`SELECT enabled, mode, detail, updated_at FROM hotspot_settings WHERE pid = ?`, pid,
	).Scan(&enabled, &s.Mode, &s.Detail, &s.UpdatedAt)
	if err == sql.ErrNoRows {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	s.Enabled = enabled == 1
	return s, nil
}

// SetHotspotEnabled flips the switch; mode/detail are reset so the daemon re-resolves them
func (db *DB) SetHotspotEnabled(pid int, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := db.Exec(
		`INSERT INTO hotspot_settings (pid, enabled, mode, detail, updated_at) VALUES (?, ?, '', '', ?)
		 ON CONFLICT(pid) DO UPDATE SET enabled = excluded.enabled, mode = '', detail = '', updated_at = excluded.updated_at`,
		pid, v, time.Now().Format(timeFmt),
	)
	return err
}

// UpdateHotspotMode records how the daemon is sampling this pid
func (db *DB) UpdateHotspotMode(pid int, mode, detail string) error {
	_, err := db.Exec(
		`UPDATE hotspot_settings SET mode = ?, detail = ? WHERE pid = ?`, mode, detail, pid,
	)
	return err
}

// ListHotspotEnabledPIDs returns the set of pids with hotspot sampling switched on
func (db *DB) ListHotspotEnabledPIDs() (map[int]bool, error) {
	rows, err := db.Query(`SELECT pid FROM hotspot_settings WHERE enabled = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[int]bool)
	for rows.Next() {
		var pid int
		if err := rows.Scan(&pid); err != nil {
			return nil, err
		}
		result[pid] = true
	}
	return result, rows.Err()
}

// SaveHotspotStacks persists a batch of folded stacks with a shared timestamp
func (db *DB) SaveHotspotStacks(pid int, mode string, rows []HotspotStackRow, at time.Time) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(
		"INSERT INTO hotspot_stacks (pid, mode, kind, stack, count, weight, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
	)
	if err != nil {
		return err
	}
	defer stmt.Close()

	ts := at.Format(timeFmt)
	for _, r := range rows {
		if _, err := stmt.Exec(pid, mode, r.Kind, r.Stack, r.Count, r.Weight, ts); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListHotspotDensePIDs returns pids that have dense samples within [from, to)
func (db *DB) ListHotspotDensePIDs(from, to time.Time) ([]int, error) {
	rows, err := db.Query(
		`SELECT DISTINCT pid FROM hotspot_stacks WHERE mode = ? AND created_at >= ? AND created_at < ?`,
		HotspotModeDense, from.Format(timeFmt), to.Format(timeFmt),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pids []int
	for rows.Next() {
		var pid int
		if err := rows.Scan(&pid); err != nil {
			return nil, err
		}
		pids = append(pids, pid)
	}
	return pids, rows.Err()
}

// AggregateHotspotHourly folds all dense samples in [from, to) into one hourly snapshot
// stamped at `to`, keeping the top limitPerKind stacks of each kind.
func (db *DB) AggregateHotspotHourly(pid int, from, to time.Time, limitPerKind int) (int64, error) {
	var total int64
	for _, kind := range []string{"cpu", "alloc"} {
		res, err := db.Exec(
			`INSERT INTO hotspot_stacks (pid, mode, kind, stack, count, weight, created_at)
			 SELECT pid, ?, kind, stack, SUM(count), SUM(weight), ?
			 FROM hotspot_stacks
			 WHERE pid = ? AND mode = ? AND kind = ? AND created_at >= ? AND created_at < ?
			 GROUP BY stack
			 ORDER BY SUM(weight) DESC
			 LIMIT ?`,
			HotspotModeHourly, to.Format(timeFmt),
			pid, HotspotModeDense, kind, from.Format(timeFmt), to.Format(timeFmt),
			limitPerKind,
		)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}

// CleanupHotspotData applies the dense (1h) / hourly (7d) retention policy
func (db *DB) CleanupHotspotData() error {
	oneHourAgo := time.Now().Add(-1 * time.Hour).Format(timeFmt)
	if _, err := db.Exec("DELETE FROM hotspot_stacks WHERE mode = ? AND created_at < ?", HotspotModeDense, oneHourAgo); err != nil {
		return err
	}
	sevenDaysAgo := time.Now().AddDate(0, 0, -7).Format(timeFmt)
	if _, err := db.Exec("DELETE FROM hotspot_stacks WHERE mode = ? AND created_at < ?", HotspotModeHourly, sevenDaysAgo); err != nil {
		return err
	}
	return nil
}

// QueryHotspotStacks returns every folded stack of a kind within the time range
func (db *DB) QueryHotspotStacks(pid int, mode, kind string, from, to time.Time) ([]HotspotStackPoint, error) {
	rows, err := db.Query(
		`SELECT created_at, stack, count, weight FROM hotspot_stacks
		 WHERE pid = ? AND mode = ? AND kind = ? AND created_at BETWEEN ? AND ?
		 ORDER BY created_at`,
		pid, mode, kind, from.Format(timeFmt), to.Format(timeFmt),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []HotspotStackPoint
	for rows.Next() {
		var p HotspotStackPoint
		if err := rows.Scan(&p.Timestamp, &p.Stack, &p.Count, &p.Weight); err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

// HotspotStats summarises what has been collected for the status panel
type HotspotStats struct {
	LastSampleAt    string `json:"lastSampleAt"`
	DenseSnapshots  int    `json:"denseSnapshots"`
	HourlySnapshots int    `json:"hourlySnapshots"`
	HasAlloc        bool   `json:"hasAlloc"`
}

// QueryHotspotStats returns collection statistics for pid
func (db *DB) QueryHotspotStats(pid int) (*HotspotStats, error) {
	st := &HotspotStats{}
	var last sql.NullString
	if err := db.QueryRow(
		`SELECT MAX(created_at) FROM hotspot_stacks WHERE pid = ?`, pid,
	).Scan(&last); err != nil {
		return nil, err
	}
	st.LastSampleAt = last.String
	if err := db.QueryRow(
		`SELECT COUNT(DISTINCT created_at) FROM hotspot_stacks WHERE pid = ? AND mode = ?`, pid, HotspotModeDense,
	).Scan(&st.DenseSnapshots); err != nil {
		return nil, err
	}
	if err := db.QueryRow(
		`SELECT COUNT(DISTINCT created_at) FROM hotspot_stacks WHERE pid = ? AND mode = ?`, pid, HotspotModeHourly,
	).Scan(&st.HourlySnapshots); err != nil {
		return nil, err
	}
	var allocRows int
	if err := db.QueryRow(
		`SELECT COUNT(1) FROM hotspot_stacks WHERE pid = ? AND kind = 'alloc' LIMIT 1`, pid,
	).Scan(&allocRows); err != nil {
		return nil, err
	}
	st.HasAlloc = allocRows > 0
	return st, nil
}
