package daemon

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"jmon/internal/collector"
	"jmon/internal/storage"
)

const (
	hotspotStackDepth    = 48  // frames kept per stack
	hotspotMaxPerKind    = 300 // folded stacks persisted per snapshot per kind (dense)
	hotspotHourlyPerKind = 500 // folded stacks kept per hourly aggregate per kind
)

// Hotspot sampling modes resolved per process
const (
	hotspotModeJFR    = "jfr"
	hotspotModeJstack = "jstack"
)

type hotspotState struct {
	mode     string
	jfrTool  string
	lastDump time.Time
}

// hotspotManager drives method-level hotspot sampling. It never calls jmap: while a
// process has hotspot enabled the heap-histogram (leak) collection is suspended for it so
// that sampling does not provoke GC activity.
type hotspotManager struct {
	db       *storage.DB
	interval time.Duration
	jfrPath  string // user-configured jfr tool (optional)
	dir      string // scratch dir for .jfr dumps
	states   map[int]*hotspotState
}

func newHotspotManager(db *storage.DB, interval time.Duration, jfrPath string) *hotspotManager {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".jmon", "jfr")
	os.MkdirAll(dir, 0755)
	// Remove dumps left behind by a previous daemon instance
	if leftovers, err := filepath.Glob(filepath.Join(dir, "*.jfr")); err == nil {
		for _, f := range leftovers {
			os.Remove(f)
		}
	}
	return &hotspotManager{
		db:       db,
		interval: interval,
		jfrPath:  jfrPath,
		dir:      dir,
		states:   make(map[int]*hotspotState),
	}
}

// refresh reconciles in-memory sampling state with the switches stored in the DB and the
// processes currently alive. It returns the set of pids with hotspot sampling active.
func (m *hotspotManager) refresh(alive map[int]bool) map[int]bool {
	enabled, err := m.db.ListHotspotEnabledPIDs()
	if err != nil {
		log.Printf("[hotspot] failed to load settings: %v", err)
		return nil
	}

	for pid, st := range m.states {
		if enabled[pid] && alive[pid] {
			continue
		}
		if st.mode == hotspotModeJFR && alive[pid] {
			if err := collector.StopJFR(pid, collector.HotspotRecordingName); err != nil {
				log.Printf("[hotspot][pid=%d] failed to stop JFR recording: %v", pid, err)
			}
		}
		delete(m.states, pid)
		log.Printf("[hotspot][pid=%d] sampling stopped", pid)
	}

	active := make(map[int]bool)
	for pid := range enabled {
		if !alive[pid] {
			continue
		}
		if _, ok := m.states[pid]; !ok {
			m.enable(pid)
		}
		active[pid] = true
	}
	return active
}

// enable resolves the best available sampling mode for pid and starts it
func (m *hotspotManager) enable(pid int) {
	st := &hotspotState{mode: hotspotModeJstack}
	detail := ""

	switch {
	case !collector.JFRSupported(pid):
		detail = "JVM has no JFR support (JDK < 11, or OpenJDK 8 before 8u262). Using jstack sampling: one RUNNABLE-stack sample per collection interval, low precision."
	default:
		tool := collector.FindJfrTool(pid, m.jfrPath)
		if tool == "" {
			detail = "JVM supports JFR but no JDK 11+ `jfr` tool is available to parse recordings (set \"jfr_path\" in ~/.jmon/config.json). Falling back to jstack sampling, low precision."
			break
		}
		if !collector.JFRRecordingActive(pid, collector.HotspotRecordingName) {
			if err := collector.StartJFR(pid, collector.HotspotRecordingName); err != nil {
				detail = fmt.Sprintf("JFR.start failed (%v). Falling back to jstack sampling, low precision.", err)
				break
			}
		}
		st.mode = hotspotModeJFR
		st.jfrTool = tool
		st.lastDump = time.Now()
		detail = fmt.Sprintf("JFR continuous recording (settings=profile, ~1-2%% overhead, no GC triggered), parsed with %s", tool)
	}

	m.states[pid] = st
	if err := m.db.UpdateHotspotMode(pid, st.mode, detail); err != nil {
		log.Printf("[hotspot][pid=%d] failed to persist mode: %v", pid, err)
	}
	log.Printf("[hotspot][pid=%d] sampling started, mode=%s", pid, st.mode)
}

// sample records one dense snapshot for pid. `threads` is this cycle's jstack result and is
// only used by the jstack fallback mode.
func (m *hotspotManager) sample(pid int, threads []collector.ThreadInfo) {
	st, ok := m.states[pid]
	if !ok {
		return
	}
	now := time.Now()
	var samples []collector.StackSample

	switch st.mode {
	case hotspotModeJFR:
		since := st.lastDump
		if since.IsZero() {
			since = now.Add(-m.interval)
		}
		file := filepath.Join(m.dir, fmt.Sprintf("hotspot-%d-%d.jfr", pid, now.UnixNano()))
		if err := collector.DumpJFR(pid, collector.HotspotRecordingName, file, now.Sub(since)+2*time.Second); err != nil {
			log.Printf("[hotspot][pid=%d] JFR.dump failed: %v", pid, err)
			// Recording may have been lost (e.g. stopped externally); re-arm it for next cycle
			if !collector.JFRRecordingActive(pid, collector.HotspotRecordingName) {
				if err := collector.StartJFR(pid, collector.HotspotRecordingName); err != nil {
					log.Printf("[hotspot][pid=%d] JFR.start retry failed: %v", pid, err)
				}
			}
			st.lastDump = now
			return
		}
		var err error
		samples, err = collector.ParseJFRHotspots(st.jfrTool, file, since, hotspotStackDepth)
		os.Remove(file)
		st.lastDump = now
		if err != nil {
			log.Printf("[hotspot][pid=%d] failed to parse JFR dump: %v", pid, err)
			return
		}
	default:
		samples = collector.FoldJstackThreads(threads, hotspotStackDepth)
	}

	rows := topPerKind(samples, hotspotMaxPerKind)
	if len(rows) == 0 {
		return
	}
	if err := m.db.SaveHotspotStacks(pid, storage.HotspotModeDense, rows, now); err != nil {
		log.Printf("[hotspot][pid=%d] failed to save samples: %v", pid, err)
	}
}

// aggregateHourly folds the previous hour's dense samples into one hourly point per pid.
// Must run before CleanupHotspotData in the same cycle so the dense rows still exist.
func (m *hotspotManager) aggregateHourly(now time.Time) {
	hourStart := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, now.Location())
	from := hourStart.Add(-time.Hour)

	pids, err := m.db.ListHotspotDensePIDs(from, hourStart)
	if err != nil {
		log.Printf("[hotspot-hourly] failed to list pids: %v", err)
		return
	}
	for _, pid := range pids {
		n, err := m.db.AggregateHotspotHourly(pid, from, hourStart, hotspotHourlyPerKind)
		if err != nil {
			log.Printf("[hotspot-hourly][pid=%d] aggregation failed: %v", pid, err)
			continue
		}
		log.Printf("[hotspot-hourly][pid=%d] saved %d stacks for %s", pid, n, hourStart.Format("15:04"))
	}
}

// shutdown stops every JFR recording jmon started
func (m *hotspotManager) shutdown() {
	for pid, st := range m.states {
		if st.mode == hotspotModeJFR && collector.IsProcessAlive(pid) {
			collector.StopJFR(pid, collector.HotspotRecordingName)
		}
	}
	m.states = make(map[int]*hotspotState)
}

// topPerKind keeps the heaviest `limit` stacks of each kind (input is sorted by weight desc)
func topPerKind(samples []collector.StackSample, limit int) []storage.HotspotStackRow {
	seen := make(map[string]int)
	var rows []storage.HotspotStackRow
	for _, s := range samples {
		if seen[s.Kind] >= limit {
			continue
		}
		seen[s.Kind]++
		rows = append(rows, storage.HotspotStackRow{
			Kind:   s.Kind,
			Stack:  s.Stack,
			Count:  s.Count,
			Weight: s.Weight,
		})
	}
	return rows
}
