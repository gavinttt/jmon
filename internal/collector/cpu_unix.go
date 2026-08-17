//go:build !windows

package collector

import (
	"bufio"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// CollectCPUStats uses ps to get process CPU/memory stats
func CollectCPUStats(pid int) (*CPUStats, error) {
	out, err := exec.Command("ps", "-p", fmt.Sprintf("%d", pid), "-o", "%cpu,%mem,rss").Output()
	if err != nil {
		return nil, fmt.Errorf("failed to run ps for pid %d: %w", pid, err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return nil, fmt.Errorf("unexpected ps output for pid %d (process may not exist)", pid)
	}
	fields := strings.Fields(lines[1])
	if len(fields) < 3 {
		return nil, fmt.Errorf("unexpected ps output format")
	}
	cpuPct, _ := strconv.ParseFloat(fields[0], 64)
	memPct, _ := strconv.ParseFloat(fields[1], 64)
	rss, _ := strconv.ParseInt(fields[2], 10, 64)
	return &CPUStats{CPUPct: cpuPct, MemPct: memPct, MemRSS: rss}, nil
}

// CollectThreadCPU returns a map of native-thread-id -> cpu%
func CollectThreadCPU(pid int) (map[int]float64, error) {
	out, err := exec.Command("ps", "-eLo", "pid,tid,%cpu").Output()
	if err != nil {
		return nil, err
	}
	result := make(map[int]float64)
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	scanner.Scan() // skip header
	pidStr := fmt.Sprintf("%d", pid)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		if fields[0] != pidStr {
			continue
		}
		tid, _ := strconv.Atoi(fields[1])
		cpu, _ := strconv.ParseFloat(fields[2], 64)
		result[tid] = cpu
	}
	return result, nil
}
