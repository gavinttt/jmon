package analyzer

import (
	"fmt"
	"sort"
	"time"

	"jmon/internal/storage"
)

// LeakResult represents a suspected memory leak
type LeakResult struct {
	ClassName        string
	OldInstanceCount int64
	NewInstanceCount int64
	OldBytes         int64
	NewBytes         int64
	InstanceGrowth   float64 // percentage
	BytesGrowth      float64 // percentage
	Severity         string  // HIGH / MEDIUM / LOW
	Reason           string
}

// DetectMemoryLeaks compares heap histograms between two time windows
func DetectMemoryLeaks(db *storage.DB, pid int, from, to time.Time) ([]LeakResult, error) {
	duration := to.Sub(from)
	// Split the time window into two halves
	mid := from.Add(duration / 2)

	comparisons, err := db.QueryHeapHistoForLeak(pid, from, mid, mid, to)
	if err != nil {
		return nil, fmt.Errorf("leak analysis failed: %w", err)
	}

	var results []LeakResult
	for _, c := range comparisons {
		if c.BytesDelta <= 0 {
			continue // only report growth
		}

		var growthPct float64
		if c.OldBytes > 0 {
			growthPct = float64(c.BytesDelta) / float64(c.OldBytes) * 100
		} else {
			growthPct = 100 // new class appeared
		}

		severity := "LOW"
		if growthPct > 100 || c.BytesDelta > 10*1024*1024 { // > 100% or > 10MB
			severity = "HIGH"
		} else if growthPct > 50 || c.BytesDelta > 5*1024*1024 { // > 50% or > 5MB
			severity = "MEDIUM"
		}

		reason := fmt.Sprintf("实例数增长 %d, 内存增长 %s (%.1f%%)",
			c.InstanceDelta, formatBytes(c.BytesDelta), growthPct)

		var instGrowth float64
		if c.OldInstanceCount > 0 {
			instGrowth = float64(c.InstanceDelta) / float64(c.OldInstanceCount) * 100
		} else {
			instGrowth = 100
		}

		results = append(results, LeakResult{
			ClassName:        c.ClassName,
			OldInstanceCount: c.OldInstanceCount,
			NewInstanceCount: c.NewInstanceCount,
			OldBytes:         c.OldBytes,
			NewBytes:         c.NewBytes,
			InstanceGrowth:   instGrowth,
			BytesGrowth:      growthPct,
			Severity:         severity,
			Reason:           reason,
		})
	}

	// Sort by bytes growth descending
	sort.Slice(results, func(i, j int) bool {
		return results[i].BytesDelta() > results[j].BytesDelta()
	})

	return results, nil
}

func (l *LeakResult) BytesDelta() int64 {
	return l.NewBytes - l.OldBytes
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// CPUHotspotResult represents a thread with high CPU usage
type CPUHotspotResult struct {
	ThreadName string
	AvgCPU     float64
	MaxCPU     float64
	State      string
	StackTrace string
	Severity   string // HIGH / MEDIUM / LOW
	Reason     string
}

// DetectCPUHotspots finds threads with sustained high CPU usage
func DetectCPUHotspots(db *storage.DB, pid int, from, to time.Time, threshold float64) ([]CPUHotspotResult, error) {
	hotspots, err := db.QueryThreadHotspots(pid, from, to, threshold)
	if err != nil {
		return nil, fmt.Errorf("CPU hotspot analysis failed: %w", err)
	}

	var results []CPUHotspotResult
	for _, h := range hotspots {
		severity := "LOW"
		if h.AvgCPU > 80 {
			severity = "HIGH"
		} else if h.AvgCPU > 50 {
			severity = "MEDIUM"
		}

		reason := fmt.Sprintf("平均 CPU %.1f%%, 峰值 %.1f%%, 状态: %s", h.AvgCPU, h.MaxCPU, h.LastState)

		results = append(results, CPUHotspotResult{
			ThreadName: h.ThreadName,
			AvgCPU:     h.AvgCPU,
			MaxCPU:     h.MaxCPU,
			State:      h.LastState,
			StackTrace: h.LastStack,
			Severity:   severity,
			Reason:     reason,
		})
	}

	return results, nil
}
