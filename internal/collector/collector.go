package collector

import (
	"bufio"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// JavaProcess represents a discovered Java process
type JavaProcess struct {
	PID     int
	Name    string
	Args    string
	JVMArgs string
}

// ThreadInfo represents a single thread's information
type ThreadInfo struct {
	Name       string
	State      string
	CPUPct     float64
	TID        int
	NID        int // native ID (OS thread ID)
	StackTrace string
}

// HeapEntry represents one class in the heap histogram
type HeapEntry struct {
	ClassName     string
	InstanceCount int64
	Bytes         int64
}

// MemoryStats represents JVM memory area sizes (in KB)
type MemoryStats struct {
	EdenUsed     float64
	EdenCap      float64
	SurvivorUsed float64
	SurvivorCap  float64
	OldUsed      float64
	OldCap       float64
	MetaUsed     float64
	MetaCap      float64
	GCCount      int64
	GCTime       float64 // ms
}

// CPUStats represents process-level CPU/memory usage
type CPUStats struct {
	CPUPct float64
	MemPct float64
	MemRSS int64 // KB
}

// CollectJavaProcesses uses jps to discover Java processes
func CollectJavaProcesses() ([]JavaProcess, error) {
	out, err := exec.Command("jps", "-lv").Output()
	if err != nil {
		return nil, fmt.Errorf("failed to run jps: %w (is JDK installed?)", err)
	}

	var procs []JavaProcess
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 3)
		if len(parts) < 2 {
			continue
		}
		pid, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		// Skip jps itself
		if strings.Contains(parts[1], "jps") || parts[1] == "jps" {
			continue
		}
		proc := JavaProcess{PID: pid, Name: parts[1]}
		if len(parts) > 2 {
			// Separate main class args from JVM args (JVM args start with -)
			rest := parts[2]
			proc.Args = rest
		}
		procs = append(procs, proc)
	}
	return procs, nil
}

// CollectThreads uses jstack to collect thread information
func CollectThreads(pid int) ([]ThreadInfo, error) {
	out, err := exec.Command("jstack", fmt.Sprintf("%d", pid)).Output()
	if err != nil {
		return nil, fmt.Errorf("failed to run jstack for pid %d: %w", pid, err)
	}
	return parseJstack(string(out)), nil
}

var threadHeaderRe = regexp.MustCompile(`^"([^"]+)"[^#]*#(\d+).*?nid=(0x[0-9a-fA-F]+|\d+)\s+(.*)`)

func parseJstack(output string) []ThreadInfo {
	var threads []ThreadInfo
	scanner := bufio.NewScanner(strings.NewReader(output))
	var current *ThreadInfo
	var stackBuf strings.Builder

	flushCurrent := func() {
		if current != nil {
			current.StackTrace = strings.TrimSpace(stackBuf.String())
			threads = append(threads, *current)
			current = nil
			stackBuf.Reset()
		}
	}

	for scanner.Scan() {
		line := scanner.Text()
		if matches := threadHeaderRe.FindStringSubmatch(line); matches != nil {
			flushCurrent()
			tid, _ := strconv.Atoi(matches[2])
			nid := parseNID(matches[3])
			current = &ThreadInfo{
				Name: matches[1],
				TID:  tid,
				NID:  nid,
			}
			// Parse state from the rest of the line
			stateStr := matches[4]
			current.State = parseThreadState(stateStr)
		} else if current != nil {
			stackBuf.WriteString(line)
			stackBuf.WriteString("\n")
			// Check for state in subsequent lines like "java.lang.Thread.State: RUNNABLE"
			if strings.Contains(line, "java.lang.Thread.State:") {
				parts := strings.SplitN(line, ":", 2)
				if len(parts) == 2 {
					current.State = strings.TrimSpace(parts[1])
				}
			}
		}
	}
	flushCurrent()
	return threads
}

func parseNID(s string) int {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "0x") {
		n, _ := strconv.ParseInt(s[2:], 16, 64)
		return int(n)
	}
	n, _ := strconv.Atoi(s)
	return n
}

func parseThreadState(s string) string {
	s = strings.TrimSpace(s)
	switch {
	case strings.Contains(s, "runnable"):
		return "RUNNABLE"
	case strings.Contains(s, "waiting on condition"):
		return "WAITING"
	case strings.Contains(s, "in Object.wait"):
		return "WAITING"
	case strings.Contains(s, "blocked"):
		return "BLOCKED"
	case strings.Contains(s, "sleeping"):
		return "TIMED_WAITING"
	default:
		return "OTHER"
	}
}

// CollectHeapHisto uses jmap -histo to get heap histogram
func CollectHeapHisto(pid int) ([]HeapEntry, error) {
	out, err := exec.Command("jmap", "-histo", fmt.Sprintf("%d", pid)).Output()
	if err != nil {
		return nil, fmt.Errorf("failed to run jmap -histo for pid %d: %w", pid, err)
	}
	return parseHisto(string(out)), nil
}

func parseHisto(output string) []HeapEntry {
	var entries []HeapEntry
	scanner := bufio.NewScanner(strings.NewReader(output))
	lineRe := regexp.MustCompile(`^\s*(\d+):\s+(\d+)\s+(\d+)\s+(.+)$`)

	for scanner.Scan() {
		line := scanner.Text()
		matches := lineRe.FindStringSubmatch(line)
		if matches == nil {
			continue
		}
		count, _ := strconv.ParseInt(matches[2], 10, 64)
		bytes, _ := strconv.ParseInt(matches[3], 10, 64)
		className := strings.TrimSpace(matches[4])
		entries = append(entries, HeapEntry{
			ClassName:     className,
			InstanceCount: count,
			Bytes:         bytes,
		})
	}
	return entries
}

// CollectMemoryStats uses jstat -gc to get memory statistics
func CollectMemoryStats(pid int) (*MemoryStats, error) {
	cmd := exec.Command("jstat", "-gc", fmt.Sprintf("%d", pid))
	out, err := cmd.Output()
	if err != nil {
		// Also try to capture stderr for better error message
		return nil, fmt.Errorf("failed to run jstat for pid %d: %w", pid, err)
	}
	return parseJstatGC(string(out))
}

func parseJstatGC(output string) (*MemoryStats, error) {
	// Normalize line endings (Windows \r\n)
	output = strings.ReplaceAll(output, "\r\n", "\n")
	output = strings.ReplaceAll(output, "\r", "\n")
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 2 {
		return nil, fmt.Errorf("unexpected jstat output (%d lines): %s", len(lines), output)
	}
	headers := strings.Fields(lines[0])
	values := strings.Fields(lines[1])
	if len(headers) != len(values) {
		return nil, fmt.Errorf("header/value mismatch: %d headers vs %d values, headers=%v", len(headers), len(values), headers)
	}

	valMap := make(map[string]float64)
	for i, h := range headers {
		v, _ := strconv.ParseFloat(values[i], 64)
		valMap[h] = v
	}

	stats := &MemoryStats{
		EdenUsed:     valMap["EU"],
		EdenCap:      valMap["EC"],
		SurvivorUsed: valMap["SU"],
		SurvivorCap:  valMap["SC"],
		OldUsed:      valMap["OU"],
		OldCap:       valMap["OC"],
		MetaUsed:     valMap["MU"],
		MetaCap:      valMap["MC"],
		GCCount:      int64(valMap["YGC"] + valMap["FGC"]),
		GCTime:       valMap["YGCT"] + valMap["FGCT"],
	}
	return stats, nil
}


