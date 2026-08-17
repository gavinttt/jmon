package daemon

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"strings"
	"time"

	"jmon/internal/collector"
	"jmon/internal/config"
	"jmon/internal/storage"
	"jmon/internal/web"
)

// Config holds daemon configuration
type Config struct {
	Interval time.Duration
	Port     int
}

// DefaultConfig returns default daemon configuration
func DefaultConfig() *Config {
	return &Config{
		Interval: 30 * time.Second,
		Port:     9810,
	}
}

func pidFile() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".jmon", "daemon.pid")
}

func logFile() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".jmon", "daemon.log")
}

// Start launches the daemon as a background process
func Start(cfg *Config) error {
	dashboardURL := fmt.Sprintf("http://localhost:%d/dashboard", cfg.Port)

	// Check if already running
	if pid, err := GetPID(); err == nil {
		fmt.Printf("Daemon already running (PID %d)\n", pid)
		fmt.Printf("Dashboard: %s\n", dashboardURL)
		openBrowserAsync(dashboardURL)
		return nil
	}

	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".jmon")
	os.MkdirAll(dir, 0755)

	lf, err := os.OpenFile(logFile(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}

	// Fork self as daemon
	exe, err := os.Executable()
	if err != nil {
		lf.Close()
		return err
	}
	// Resolve symlinks and fallback to os.Args[0] if the resolved path doesn't exist
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil || !fileExists(exe) {
		if len(os.Args) > 0 {
			if abs, err := filepath.Abs(os.Args[0]); err == nil && fileExists(abs) {
				exe = abs
			}
		}
	}
	if !fileExists(exe) {
		lf.Close()
		return fmt.Errorf("cannot locate executable, please build with 'go build' instead of 'go run'")
	}

	args := []string{"_run",
		"--interval", fmt.Sprintf("%d", int(cfg.Interval.Seconds())),
		"--port", fmt.Sprintf("%d", cfg.Port),
	}

	cmd := exec.Command(exe, args...)
	cmd.Stdout = lf
	cmd.Stderr = lf
	setProcAttr(cmd)

	if err := cmd.Start(); err != nil {
		lf.Close()
		return fmt.Errorf("failed to start daemon: %w", err)
	}

	// Write PID file
	if err := os.WriteFile(pidFile(), []byte(fmt.Sprintf("%d", cmd.Process.Pid)), 0644); err != nil {
		cmd.Process.Kill()
		lf.Close()
		return err
	}

	lf.Close()
	fmt.Printf("Daemon started (PID %d), interval %s\n", cmd.Process.Pid, cfg.Interval)
	fmt.Printf("Dashboard: %s\n", dashboardURL)
	fmt.Printf("Log: %s\n", logFile())

	// Open browser after web server is ready (detached shell process)
	openBrowserAsync(dashboardURL)

	return nil
}

// Stop terminates the daemon process
func Stop() error {
	pid, err := GetPID()
	if err != nil {
		return fmt.Errorf("daemon not running")
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		os.Remove(pidFile())
		return fmt.Errorf("daemon process not found")
	}

	if err := stopProcess(proc); err != nil {
		proc.Kill()
	}

	os.Remove(pidFile())
	fmt.Println("Daemon stopped")
	return nil
}

// Status returns daemon status
func Status() (int, bool, error) {
	pid, err := GetPID()
	if err != nil {
		return 0, false, err
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		os.Remove(pidFile())
		return pid, false, nil
	}
	if !isProcessAlive(proc) {
		os.Remove(pidFile())
		return pid, false, nil
	}
	return pid, true, nil
}

// GetPID reads the daemon PID from the pid file
func GetPID() (int, error) {
	data, err := os.ReadFile(pidFile())
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Run is the actual daemon loop (called after fork)
func Run(ctx context.Context, cfg *Config) error {
	db, err := storage.Open()
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close()

	// Cleanup old data on startup
	log.Println("Cleaning up data older than 30 days...")
	db.Cleanup(30)
	db.CleanupLeakData()

	// Load auth config
	appCfg, _ := config.Load()
	log.Printf("Auth: username=%s", appCfg.Username)

	// Start web server
	server := web.NewServer(db, cfg.Port, appCfg.Username, appCfg.Password)
	go func() {
		log.Printf("Web server starting on port %d", cfg.Port)
		if err := server.Start(); err != nil {
			log.Printf("Web server error: %v", err)
		}
	}()

	// Collection loop
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	log.Printf("Daemon started, collecting every %s", cfg.Interval)

	// Collect immediately on start
	collectOnce(db)
	collectLeakHourly(db) // initial hourly snapshot
	lastHourlyHour := time.Now().Hour()

	for {
		select {
		case <-ctx.Done():
			log.Println("Daemon shutting down...")
			server.Stop()
			return nil
		case <-ticker.C:
			collectOnce(db)
			// Hourly leak snapshot at the top of the hour (e.g. 1:00, 2:00, ...)
			nowHour := time.Now().Hour()
			if nowHour != lastHourlyHour {
				collectLeakHourly(db)
				lastHourlyHour = nowHour
			}
			// Periodic leak data cleanup
			db.CleanupLeakData()
		}
	}
}

func collectOnce(db *storage.DB) {
	procs, err := collector.CollectJavaProcesses()
	if err != nil {
		log.Printf("Failed to discover Java processes: %v", err)
		return
	}

	for _, proc := range procs {
		// Skip if process already exited
		if !collector.IsProcessAlive(proc.PID) {
			log.Printf("[pid=%d] process exited, skipping", proc.PID)
			continue
		}

		// Save process info
		db.SaveProcess(proc.PID, proc.Name, proc.Args)

		// Collect threads
		threads, err := collector.CollectThreads(proc.PID)
		if err != nil {
			log.Printf("[pid=%d] jstack failed: %v", proc.PID, err)
		} else {
			// Enrich with CPU per thread
			threadCPU, _ := collector.CollectThreadCPU(proc.PID)
			var rows []storage.ThreadRow
			for _, t := range threads {
				cpuPct := threadCPU[t.NID]
				rows = append(rows, storage.ThreadRow{
					Name:       t.Name,
					State:      t.State,
					StackTrace: t.StackTrace,
					CPUPct:     cpuPct,
					TID:        t.TID,
					NID:        t.NID,
				})
			}
			if err := db.SaveThreadSnapshot(proc.PID, rows); err != nil {
				log.Printf("[pid=%d] failed to save threads: %v", proc.PID, err)
			}
		}

		// Collect heap histogram (limit to top 300 for leak detection)
		heapEntries, err := collector.CollectHeapHisto(proc.PID)
		if err != nil {
			log.Printf("[pid=%d] jmap failed: %v", proc.PID, err)
		} else {
			// Save top 100 to heap_histo (general monitoring)
			heapLimit := 100
			if len(heapEntries) < heapLimit {
				heapLimit = len(heapEntries)
			}
			var rows []storage.HeapHistoRow
			for _, h := range heapEntries[:heapLimit] {
				rows = append(rows, storage.HeapHistoRow{
					ClassName:     h.ClassName,
					InstanceCount: h.InstanceCount,
					Bytes:         h.Bytes,
				})
			}
			if err := db.SaveHeapHisto(proc.PID, rows); err != nil {
				log.Printf("[pid=%d] failed to save heap histo: %v", proc.PID, err)
			}

			// Save top 300 to leak_histo (dense mode - every 30s)
			leakLimit := 300
			if len(heapEntries) < leakLimit {
				leakLimit = len(heapEntries)
			}
			var leakRows []storage.LeakHistoRow
			for _, h := range heapEntries[:leakLimit] {
				leakRows = append(leakRows, storage.LeakHistoRow{
					ClassName:     h.ClassName,
					InstanceCount: h.InstanceCount,
					Bytes:         h.Bytes,
				})
			}
			if err := db.SaveLeakHisto(proc.PID, "dense", leakRows); err != nil {
				log.Printf("[pid=%d] failed to save dense leak: %v", proc.PID, err)
			}
		}

		// Collect memory stats
		memStats, err := collector.CollectMemoryStats(proc.PID)
		if err != nil {
			log.Printf("[pid=%d] jstat failed: %v", proc.PID, err)
		} else {
			if memStats.EdenCap == 0 && memStats.OldCap == 0 && memStats.MetaCap == 0 {
				log.Printf("[pid=%d] jstat returned all zeros - check JDK compatibility", proc.PID)
			}
			if err := db.SaveMemorySnapshot(proc.PID, storage.MemoryRow{
				EdenUsed: memStats.EdenUsed, EdenCap: memStats.EdenCap,
				SurvivorUsed: memStats.SurvivorUsed, SurvivorCap: memStats.SurvivorCap,
				OldUsed: memStats.OldUsed, OldCap: memStats.OldCap,
				MetaUsed: memStats.MetaUsed, MetaCap: memStats.MetaCap,
				GCCount: memStats.GCCount, GCTime: memStats.GCTime,
			}); err != nil {
				log.Printf("[pid=%d] failed to save memory: %v", proc.PID, err)
			}
		}

		// Collect CPU stats
		cpuStats, err := collector.CollectCPUStats(proc.PID)
		if err != nil {
			log.Printf("[pid=%d] ps failed: %v", proc.PID, err)
		} else {
			if err := db.SaveCPUSnapshot(proc.PID, cpuStats.CPUPct, cpuStats.MemPct, cpuStats.MemRSS); err != nil {
				log.Printf("[pid=%d] failed to save CPU: %v", proc.PID, err)
			}
		}
	}

	log.Printf("Collection complete: %d Java processes", len(procs))
}

// collectLeakHourly collects heap histogram for hourly leak detection (top 300)
func collectLeakHourly(db *storage.DB) {
	procs, err := collector.CollectJavaProcesses()
	if err != nil {
		log.Printf("[leak-hourly] Failed to discover Java processes: %v", err)
		return
	}

	for _, proc := range procs {
		if !collector.IsProcessAlive(proc.PID) {
			continue
		}

		heapEntries, err := collector.CollectHeapHisto(proc.PID)
		if err != nil {
			log.Printf("[leak-hourly][pid=%d] jmap failed: %v", proc.PID, err)
			continue
		}

		limit := 300
		if len(heapEntries) < limit {
			limit = len(heapEntries)
		}
		var leakRows []storage.LeakHistoRow
		for _, h := range heapEntries[:limit] {
			leakRows = append(leakRows, storage.LeakHistoRow{
				ClassName:     h.ClassName,
				InstanceCount: h.InstanceCount,
				Bytes:         h.Bytes,
			})
		}
		if err := db.SaveLeakHisto(proc.PID, "hourly", leakRows); err != nil {
			log.Printf("[leak-hourly][pid=%d] failed to save: %v", proc.PID, err)
		}
	}
	log.Printf("[leak-hourly] Saved hourly snapshots for %d processes", len(procs))
}
