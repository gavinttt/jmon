package storage

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// timeFmt is the canonical time format with timezone offset for correct cross-timezone handling
const timeFmt = "2006-01-02T15:04:05-07:00"

// DB wraps the SQLite database
type DB struct {
	*sql.DB
}

// Open opens or creates the jmon database
func Open() (*DB, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, ".jmon")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	dbPath := filepath.Join(dir, "jmon.db")
	dsn := dbPath + "?_journal_mode=WAL&_busy_timeout=5000"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db := &DB{sqlDB}
	if err := db.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func (db *DB) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS processes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			pid INTEGER NOT NULL,
			name TEXT NOT NULL,
			args TEXT,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS thread_snapshots (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			pid INTEGER NOT NULL,
			thread_name TEXT,
			state TEXT,
			stack_trace TEXT,
			cpu_pct REAL DEFAULT 0,
			tid INTEGER,
			nid INTEGER,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS memory_snapshots (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			pid INTEGER NOT NULL,
			eden_used REAL,
			eden_cap REAL,
			survivor_used REAL,
			survivor_cap REAL,
			old_used REAL,
			old_cap REAL,
			meta_used REAL,
			meta_cap REAL,
			gc_count INTEGER,
			gc_time REAL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS heap_histo (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			pid INTEGER NOT NULL,
			class_name TEXT NOT NULL,
			instance_count INTEGER,
			bytes INTEGER,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS cpu_snapshots (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			pid INTEGER NOT NULL,
			cpu_pct REAL,
			mem_pct REAL,
			mem_rss INTEGER,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_thread_pid_ts ON thread_snapshots(pid, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_memory_pid_ts ON memory_snapshots(pid, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_heap_pid_ts ON heap_histo(pid, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_cpu_pid_ts ON cpu_snapshots(pid, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_proc_pid_ts ON processes(pid, created_at)`,
		`CREATE TABLE IF NOT EXISTS leak_histo (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			pid INTEGER NOT NULL,
			class_name TEXT NOT NULL,
			instance_count INTEGER,
			bytes INTEGER,
			mode TEXT NOT NULL DEFAULT 'hourly',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_leak_pid_mode_ts ON leak_histo(pid, mode, created_at)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("migration failed: %w\nSQL: %s", err, s)
		}
	}
	return nil
}

// Cleanup removes data older than the specified number of days
func (db *DB) Cleanup(days int) error {
	cutoff := time.Now().AddDate(0, 0, -days).Format(timeFmt)
	tables := []string{"thread_snapshots", "memory_snapshots", "heap_histo", "cpu_snapshots", "processes"}
	for _, t := range tables {
		if _, err := db.Exec(fmt.Sprintf("DELETE FROM %s WHERE created_at < ?", t), cutoff); err != nil {
			return err
		}
	}
	return nil
}

// SaveProcess saves a process snapshot
func (db *DB) SaveProcess(pid int, name, args string) error {
	_, err := db.Exec(
		"INSERT INTO processes (pid, name, args, created_at) VALUES (?, ?, ?, ?)",
		pid, name, args, time.Now().Format(timeFmt),
	)
	return err
}

// SaveThreadSnapshot saves a batch of thread snapshots
func (db *DB) SaveThreadSnapshot(pid int, threads []ThreadRow) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(
		"INSERT INTO thread_snapshots (pid, thread_name, state, stack_trace, cpu_pct, tid, nid, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
	)
	if err != nil {
		return err
	}
	defer stmt.Close()

	now := time.Now().Format(timeFmt)
	for _, t := range threads {
		if _, err := stmt.Exec(pid, t.Name, t.State, t.StackTrace, t.CPUPct, t.TID, t.NID, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SaveMemorySnapshot saves a memory statistics snapshot
func (db *DB) SaveMemorySnapshot(pid int, m MemoryRow) error {
	_, err := db.Exec(
		`INSERT INTO memory_snapshots (pid, eden_used, eden_cap, survivor_used, survivor_cap,
		 old_used, old_cap, meta_used, meta_cap, gc_count, gc_time, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		pid, m.EdenUsed, m.EdenCap, m.SurvivorUsed, m.SurvivorCap,
		m.OldUsed, m.OldCap, m.MetaUsed, m.MetaCap, m.GCCount, m.GCTime,
		time.Now().Format(timeFmt),
	)
	return err
}

// SaveHeapHisto saves heap histogram entries
func (db *DB) SaveHeapHisto(pid int, entries []HeapHistoRow) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(
		"INSERT INTO heap_histo (pid, class_name, instance_count, bytes, created_at) VALUES (?, ?, ?, ?, ?)",
	)
	if err != nil {
		return err
	}
	defer stmt.Close()

	now := time.Now().Format(timeFmt)
	for _, h := range entries {
		if _, err := stmt.Exec(pid, h.ClassName, h.InstanceCount, h.Bytes, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SaveCPUSnapshot saves a CPU/memory usage snapshot
func (db *DB) SaveCPUSnapshot(pid int, cpuPct, memPct float64, memRSS int64) error {
	_, err := db.Exec(
		"INSERT INTO cpu_snapshots (pid, cpu_pct, mem_pct, mem_rss, created_at) VALUES (?, ?, ?, ?, ?)",
		pid, cpuPct, memPct, memRSS, time.Now().Format(timeFmt),
	)
	return err
}

// Row types for batch insertion
type ThreadRow struct {
	Name       string
	State      string
	StackTrace string
	CPUPct     float64
	TID        int
	NID        int
}

type MemoryRow struct {
	EdenUsed, EdenCap         float64
	SurvivorUsed, SurvivorCap float64
	OldUsed, OldCap           float64
	MetaUsed, MetaCap         float64
	GCCount                   int64
	GCTime                    float64
}

type HeapHistoRow struct {
	ClassName     string
	InstanceCount int64
	Bytes         int64
}

// --- Query methods ---

// MemoryHistory represents a point-in-time memory snapshot for charting
type MemoryHistory struct {
	Timestamp      string
	EdenUsed       float64
	SurvivorUsed   float64
	OldUsed        float64
	MetaUsed       float64
	EdenCap        float64
	SurvivorCap    float64
	OldCap         float64
	MetaCap        float64
}

// CPUHistory represents a point-in-time CPU snapshot for charting
type CPUHistory struct {
	Timestamp string
	CPUPct    float64
	MemPct    float64
	MemRSS    int64
}

// ThreadHistory represents a thread state at a point in time
type ThreadHistory struct {
	Timestamp  string
	ThreadName string
	State      string
	CPUPct     float64
	StackTrace string
}

// HeapHistoResult represents a heap histogram at a point in time
type HeapHistoResult struct {
	Timestamp     string
	ClassName     string
	InstanceCount int64
	Bytes         int64
}

// QueryMemoryHistory retrieves memory snapshots for a pid within a time range
func (db *DB) QueryMemoryHistory(pid int, from, to time.Time) ([]MemoryHistory, error) {
	rows, err := db.Query(
		`SELECT created_at, eden_used, survivor_used, old_used, meta_used,
		        eden_cap, survivor_cap, old_cap, meta_cap
		 FROM memory_snapshots WHERE pid = ? AND created_at BETWEEN ? AND ?
		 ORDER BY created_at`,
		pid,
		from.Format(timeFmt),
		to.Format(timeFmt),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []MemoryHistory
	for rows.Next() {
		var h MemoryHistory
		if err := rows.Scan(&h.Timestamp, &h.EdenUsed, &h.SurvivorUsed, &h.OldUsed, &h.MetaUsed,
			&h.EdenCap, &h.SurvivorCap, &h.OldCap, &h.MetaCap); err != nil {
			return nil, err
		}
		result = append(result, h)
	}
	return result, rows.Err()
}

// QueryCPUHistory retrieves CPU snapshots for a pid within a time range
func (db *DB) QueryCPUHistory(pid int, from, to time.Time) ([]CPUHistory, error) {
	rows, err := db.Query(
		`SELECT created_at, cpu_pct, mem_pct, mem_rss
		 FROM cpu_snapshots WHERE pid = ? AND created_at BETWEEN ? AND ?
		 ORDER BY created_at`,
		pid,
		from.Format(timeFmt),
		to.Format(timeFmt),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []CPUHistory
	for rows.Next() {
		var h CPUHistory
		if err := rows.Scan(&h.Timestamp, &h.CPUPct, &h.MemPct, &h.MemRSS); err != nil {
			return nil, err
		}
		result = append(result, h)
	}
	return result, rows.Err()
}

// QueryLatestThreads retrieves the most recent thread snapshots for a pid
func (db *DB) QueryLatestThreads(pid int, limit int) ([]ThreadHistory, error) {
	// Get the latest timestamp for this pid
	var latestTS string
	err := db.QueryRow(
		"SELECT MAX(created_at) FROM thread_snapshots WHERE pid = ?", pid,
	).Scan(&latestTS)
	if err != nil || latestTS == "" {
		return nil, err
	}

	rows, err := db.Query(
		`SELECT created_at, thread_name, state, cpu_pct, stack_trace
		 FROM thread_snapshots WHERE pid = ? AND created_at = ?
		 ORDER BY cpu_pct DESC LIMIT ?`,
		pid, latestTS, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []ThreadHistory
	for rows.Next() {
		var h ThreadHistory
		if err := rows.Scan(&h.Timestamp, &h.ThreadName, &h.State, &h.CPUPct, &h.StackTrace); err != nil {
			return nil, err
		}
		result = append(result, h)
	}
	return result, rows.Err()
}

// QueryLatestHeapHisto retrieves the most recent heap histogram for a pid
func (db *DB) QueryLatestHeapHisto(pid int, limit int) ([]HeapHistoResult, error) {
	var latestTS string
	err := db.QueryRow(
		"SELECT MAX(created_at) FROM heap_histo WHERE pid = ?", pid,
	).Scan(&latestTS)
	if err != nil || latestTS == "" {
		return nil, err
	}

	rows, err := db.Query(
		`SELECT created_at, class_name, instance_count, bytes
		 FROM heap_histo WHERE pid = ? AND created_at = ?
		 ORDER BY bytes DESC LIMIT ?`,
		pid, latestTS, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []HeapHistoResult
	for rows.Next() {
		var h HeapHistoResult
		if err := rows.Scan(&h.Timestamp, &h.ClassName, &h.InstanceCount, &h.Bytes); err != nil {
			return nil, err
		}
		result = append(result, h)
	}
	return result, rows.Err()
}

// HeapHistoComparison for leak detection
type HeapHistoComparison struct {
	ClassName        string
	OldInstanceCount int64
	NewInstanceCount int64
	OldBytes         int64
	NewBytes         int64
	InstanceDelta    int64
	BytesDelta       int64
}

// QueryHeapHistoForLeak compares two time windows for leak detection
func (db *DB) QueryHeapHistoForLeak(pid int, fromOld, toOld, fromNew, toNew time.Time) ([]HeapHistoComparison, error) {
	query := `
		SELECT
			COALESCE(o.class_name, n.class_name) AS class_name,
			COALESCE(o.total_count, 0) AS old_count,
			COALESCE(n.total_count, 0) AS new_count,
			COALESCE(o.total_bytes, 0) AS old_bytes,
			COALESCE(n.total_bytes, 0) AS new_bytes
		FROM (
			SELECT class_name, AVG(instance_count) AS total_count, AVG(bytes) AS total_bytes
			FROM heap_histo WHERE pid = ? AND created_at BETWEEN ? AND ?
			GROUP BY class_name
		) o
		FULL OUTER JOIN (
			SELECT class_name, AVG(instance_count) AS total_count, AVG(bytes) AS total_bytes
			FROM heap_histo WHERE pid = ? AND created_at BETWEEN ? AND ?
			GROUP BY class_name
		) n ON o.class_name = n.class_name
		ORDER BY (COALESCE(n.total_bytes, 0) - COALESCE(o.total_bytes, 0)) DESC
		LIMIT 50
	`
	rows, err := db.Query(query,
		pid, fromOld.Format(timeFmt), toOld.Format(timeFmt),
		pid, fromNew.Format(timeFmt), toNew.Format(timeFmt),
	)
	if err != nil {
		// SQLite doesn't support FULL OUTER JOIN, use a workaround
		return db.queryHeapHistoForLeakCompat(pid, fromOld, toOld, fromNew, toNew)
	}
	defer rows.Close()

	var result []HeapHistoComparison
	for rows.Next() {
		var h HeapHistoComparison
		if err := rows.Scan(&h.ClassName, &h.OldInstanceCount, &h.NewInstanceCount,
			&h.OldBytes, &h.NewBytes); err != nil {
			return nil, err
		}
		h.InstanceDelta = h.NewInstanceCount - h.OldInstanceCount
		h.BytesDelta = h.NewBytes - h.OldBytes
		result = append(result, h)
	}
	return result, rows.Err()
}

// SQLite-compatible leak detection using UNION
func (db *DB) queryHeapHistoForLeakCompat(pid int, fromOld, toOld, fromNew, toNew time.Time) ([]HeapHistoComparison, error) {
	type classStats struct {
		Count int64
		Bytes int64
	}
	oldStats := make(map[string]classStats)
	newStats := make(map[string]classStats)

	// Query old period
	rows, err := db.Query(
		`SELECT class_name, AVG(instance_count), AVG(bytes)
		 FROM heap_histo WHERE pid = ? AND created_at BETWEEN ? AND ?
		 GROUP BY class_name`,
		pid, fromOld.Format(timeFmt), toOld.Format(timeFmt),
	)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var cn string
		var c, b int64
		rows.Scan(&cn, &c, &b)
		oldStats[cn] = classStats{c, b}
	}
	rows.Close()

	// Query new period
	rows, err = db.Query(
		`SELECT class_name, AVG(instance_count), AVG(bytes)
		 FROM heap_histo WHERE pid = ? AND created_at BETWEEN ? AND ?
		 GROUP BY class_name`,
		pid, fromNew.Format(timeFmt), toNew.Format(timeFmt),
	)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var cn string
		var c, b int64
		rows.Scan(&cn, &c, &b)
		newStats[cn] = classStats{c, b}
	}
	rows.Close()

	// Merge
	allClasses := make(map[string]bool)
	for k := range oldStats {
		allClasses[k] = true
	}
	for k := range newStats {
		allClasses[k] = true
	}

	var result []HeapHistoComparison
	for cn := range allClasses {
		o := oldStats[cn]
		n := newStats[cn]
		result = append(result, HeapHistoComparison{
			ClassName:        cn,
			OldInstanceCount: o.Count,
			NewInstanceCount: n.Count,
			OldBytes:         o.Bytes,
			NewBytes:         n.Bytes,
			InstanceDelta:    n.Count - o.Count,
			BytesDelta:       n.Bytes - o.Bytes,
		})
	}

	// Sort by BytesDelta descending
	for i := 0; i < len(result); i++ {
		for j := i + 1; j < len(result); j++ {
			if result[j].BytesDelta > result[i].BytesDelta {
				result[i], result[j] = result[j], result[i]
			}
		}
	}
	if len(result) > 50 {
		result = result[:50]
	}
	return result, nil
}

// QueryThreadHotspots finds threads with high CPU over a time range
type ThreadHotspot struct {
	ThreadName string
	AvgCPU     float64
	MaxCPU     float64
	LastState  string
	LastStack  string
}

func (db *DB) QueryThreadHotspots(pid int, from, to time.Time, threshold float64) ([]ThreadHotspot, error) {
	rows, err := db.Query(
		`SELECT thread_name, AVG(cpu_pct), MAX(cpu_pct),
			(SELECT state FROM thread_snapshots t2 WHERE t2.pid = t1.pid AND t2.thread_name = t1.thread_name ORDER BY created_at DESC LIMIT 1),
			(SELECT stack_trace FROM thread_snapshots t3 WHERE t3.pid = t1.pid AND t3.thread_name = t1.thread_name ORDER BY created_at DESC LIMIT 1)
		 FROM thread_snapshots t1
		 WHERE pid = ? AND created_at BETWEEN ? AND ?
		 GROUP BY thread_name
		 HAVING MAX(cpu_pct) >= ?
		 ORDER BY AVG(cpu_pct) DESC`,
		pid,
		from.Format(timeFmt),
		to.Format(timeFmt),
		threshold,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []ThreadHotspot
	for rows.Next() {
		var h ThreadHotspot
		if err := rows.Scan(&h.ThreadName, &h.AvgCPU, &h.MaxCPU, &h.LastState, &h.LastStack); err != nil {
			return nil, err
		}
		result = append(result, h)
	}
	return result, rows.Err()
}

// --- Leak Detection (Smart) ---

// LeakHistoRow represents a single class entry for leak detection
type LeakHistoRow struct {
	ClassName     string
	InstanceCount int64
	Bytes         int64
}

// SaveLeakHisto saves a batch of heap entries for leak detection with a given mode
func (db *DB) SaveLeakHisto(pid int, mode string, entries []LeakHistoRow) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(
		"INSERT INTO leak_histo (pid, class_name, instance_count, bytes, mode, created_at) VALUES (?, ?, ?, ?, ?, ?)",
	)
	if err != nil {
		return err
	}
	defer stmt.Close()

	now := time.Now().Format(timeFmt)
	for _, e := range entries {
		if _, err := stmt.Exec(pid, e.ClassName, e.InstanceCount, e.Bytes, mode, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CleanupLeakData removes old leak detection data based on mode
func (db *DB) CleanupLeakData() error {
	oneHourAgo := time.Now().Add(-1 * time.Hour).Format(timeFmt)
	if _, err := db.Exec("DELETE FROM leak_histo WHERE mode = 'dense' AND created_at < ?", oneHourAgo); err != nil {
		return err
	}
	sevenDaysAgo := time.Now().AddDate(0, 0, -7).Format(timeFmt)
	if _, err := db.Exec("DELETE FROM leak_histo WHERE mode = 'hourly' AND created_at < ?", sevenDaysAgo); err != nil {
		return err
	}
	return nil
}

// LeakHistoPoint represents a class's data at a point in time
type LeakHistoPoint struct {
	Timestamp     string `json:"timestamp"`
	ClassName     string `json:"className"`
	InstanceCount int64  `json:"instanceCount"`
	Bytes         int64  `json:"bytes"`
}

// LeakLatestRow represents a class row in the latest snapshot
type LeakLatestRow struct {
	ClassName     string `json:"class"`
	InstanceCount int64  `json:"count"`
	Bytes         int64  `json:"bytes"`
}

// QueryLeakLatest returns the most recent snapshot's classes for a given mode
func (db *DB) QueryLeakLatest(pid int, mode string) ([]LeakLatestRow, error) {
	var ts string
	err := db.QueryRow(
		`SELECT MAX(created_at) FROM leak_histo WHERE pid = ? AND mode = ?`, pid, mode,
	).Scan(&ts)
	if err != nil {
		return nil, nil
	}
	if ts == "" {
		return nil, nil
	}
	rows, err := db.Query(
		`SELECT class_name, instance_count, bytes FROM leak_histo
		 WHERE pid = ? AND mode = ? AND created_at = ?
		 ORDER BY bytes DESC`, pid, mode, ts,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []LeakLatestRow
	for rows.Next() {
		var r LeakLatestRow
		rows.Scan(&r.ClassName, &r.InstanceCount, &r.Bytes)
		result = append(result, r)
	}
	return result, nil
}

// QueryLeakClasses returns time series for specific classes
func (db *DB) QueryLeakClasses(pid int, mode string, from, to time.Time, classes []string) ([]LeakHistoPoint, error) {
	if len(classes) == 0 {
		return nil, nil
	}
	fromStr := from.Format(timeFmt)
	toStr := to.Format(timeFmt)

	placeholders := ""
	args := []interface{}{pid, mode, fromStr, toStr}
	for i, cn := range classes {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
		args = append(args, cn)
	}

	query := fmt.Sprintf(
		`SELECT created_at, class_name, instance_count, bytes
		 FROM leak_histo
		 WHERE pid = ? AND mode = ? AND created_at BETWEEN ? AND ?
		   AND class_name IN (%s)
		 ORDER BY created_at, bytes DESC`, placeholders,
	)

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []LeakHistoPoint
	for rows.Next() {
		var p LeakHistoPoint
		rows.Scan(&p.Timestamp, &p.ClassName, &p.InstanceCount, &p.Bytes)
		result = append(result, p)
	}
	return result, nil
}

// QueryLeakHistoTimeSeries returns top N classes by byte change over a time range
func (db *DB) QueryLeakHistoTimeSeries(pid int, mode string, from, to time.Time, topN int) ([]LeakHistoPoint, error) {
	fromStr := from.Format(timeFmt)
	toStr := to.Format(timeFmt)

	topQuery := `
		SELECT class_name, MAX(bytes) - MIN(bytes) AS byte_delta
		FROM leak_histo
		WHERE pid = ? AND mode = ? AND created_at BETWEEN ? AND ?
		GROUP BY class_name
		ORDER BY byte_delta DESC
		LIMIT ?
	`
	rows, err := db.Query(topQuery, pid, mode, fromStr, toStr, topN)
	if err != nil {
		return nil, err
	}
	var topClasses []string
	for rows.Next() {
		var cn string
		var delta int64
		rows.Scan(&cn, &delta)
		topClasses = append(topClasses, cn)
	}
	rows.Close()

	if len(topClasses) == 0 {
		return nil, nil
	}

	placeholders := ""
	args := []interface{}{pid, mode, fromStr, toStr}
	for i, cn := range topClasses {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
		args = append(args, cn)
	}

	query := fmt.Sprintf(
		`SELECT created_at, class_name, instance_count, bytes
		 FROM leak_histo
		 WHERE pid = ? AND mode = ? AND created_at BETWEEN ? AND ?
		 AND class_name IN (%s)
		 ORDER BY created_at, bytes DESC`,
		placeholders,
	)

	dataRows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer dataRows.Close()

	var result []LeakHistoPoint
	for dataRows.Next() {
		var p LeakHistoPoint
		if err := dataRows.Scan(&p.Timestamp, &p.ClassName, &p.InstanceCount, &p.Bytes); err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, dataRows.Err()
}
