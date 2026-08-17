//go:build windows

package collector

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// CollectCPUStats uses PowerShell to get process CPU/memory stats on Windows
func CollectCPUStats(pid int) (*CPUStats, error) {
	script := fmt.Sprintf(`$p = Get-Process -Id %d -ErrorAction Stop; $ws = $p.WorkingSet64; $cpu = $p.CPU; "$cpu $ws"`, pid)
	out, err := exec.Command("powershell", "-NoProfile", "-Command", script).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("powershell failed for pid %d: %w, output: %s", pid, err, string(out))
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return nil, fmt.Errorf("no stats for pid %d (process may have exited)", pid)
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return nil, fmt.Errorf("unexpected output for pid %d: %q", pid, line)
	}
	cpuTime, _ := strconv.ParseFloat(fields[0], 64)
	ws, _ := strconv.ParseInt(fields[1], 10, 64)
	rss := ws / 1024 // bytes -> KB
	return &CPUStats{CPUPct: cpuTime, MemPct: 0, MemRSS: rss}, nil
}

// CollectThreadCPU is not available on Windows, returns empty map
func CollectThreadCPU(pid int) (map[int]float64, error) {
	return make(map[int]float64), nil
}
