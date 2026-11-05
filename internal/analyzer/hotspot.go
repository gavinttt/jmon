package analyzer

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"jmon/internal/storage"
)

// HotspotFrame is a method's aggregated sampling weight
type HotspotFrame struct {
	Name     string  `json:"name"`
	Self     int64   `json:"self"`  // weight of samples where this frame is the leaf
	Total    int64   `json:"total"` // weight of samples containing this frame anywhere
	SelfPct  float64 `json:"selfPct"`
	TotalPct float64 `json:"totalPct"`
}

// FlameNode is a merged call-tree node for the flame graph
type FlameNode struct {
	Name     string       `json:"name"`
	Value    int64        `json:"value"`
	Children []*FlameNode `json:"children,omitempty"`
}

// HotspotSeries is one method's weight per snapshot (aligned with HotspotReport.Timestamps)
type HotspotSeries struct {
	Name   string  `json:"name"`
	Values []int64 `json:"values"`
}

// HotspotReport is the full method-level hotspot analysis for a time range
type HotspotReport struct {
	Kind         string          `json:"kind"`
	Mode         string          `json:"mode"`
	TotalWeight  int64           `json:"totalWeight"`
	TotalCount   int64           `json:"totalCount"`
	Snapshots    int             `json:"snapshots"`
	UniqueStacks int             `json:"uniqueStacks"`
	Self         []HotspotFrame  `json:"self"`
	Total        []HotspotFrame  `json:"total"`
	Flame        *FlameNode      `json:"flame"`
	Timestamps   []string        `json:"timestamps"`
	Series       []HotspotSeries `json:"series"`
}

const (
	flameMinShare = 0.001 // drop call-tree nodes below 0.1% of total weight
	seriesTopN    = 5
)

// AnalyzeHotspots aggregates folded stacks into self/total rankings, a merged call tree and
// a per-snapshot timeline of the hottest methods.
func AnalyzeHotspots(db *storage.DB, pid int, mode, kind string, from, to time.Time, topN int) (*HotspotReport, error) {
	rows, err := db.QueryHotspotStacks(pid, mode, kind, from, to)
	if err != nil {
		return nil, fmt.Errorf("hotspot analysis failed: %w", err)
	}

	report := &HotspotReport{Kind: kind, Mode: mode, Self: []HotspotFrame{}, Total: []HotspotFrame{}}
	if len(rows) == 0 {
		return report, nil
	}

	stackWeight := make(map[string]int64)
	selfWeight := make(map[string]int64)
	totalWeight := make(map[string]int64)
	perSnapshotLeaf := make(map[string]map[string]int64)
	var timestamps []string

	for _, r := range rows {
		if r.Stack == "" {
			continue
		}
		report.TotalWeight += r.Weight
		report.TotalCount += r.Count
		stackWeight[r.Stack] += r.Weight

		frames := strings.Split(r.Stack, ";")
		leaf := frames[len(frames)-1]
		selfWeight[leaf] += r.Weight

		seen := make(map[string]bool, len(frames))
		for _, f := range frames {
			if !seen[f] {
				seen[f] = true
				totalWeight[f] += r.Weight
			}
		}

		if _, ok := perSnapshotLeaf[r.Timestamp]; !ok {
			perSnapshotLeaf[r.Timestamp] = make(map[string]int64)
			timestamps = append(timestamps, r.Timestamp)
		}
		perSnapshotLeaf[r.Timestamp][leaf] += r.Weight
	}
	sort.Strings(timestamps)

	report.Snapshots = len(timestamps)
	report.UniqueStacks = len(stackWeight)
	report.Self = rankFrames(selfWeight, totalWeight, report.TotalWeight, topN, true)
	report.Total = rankFrames(selfWeight, totalWeight, report.TotalWeight, topN, false)
	report.Flame = buildFlame(stackWeight, report.TotalWeight)
	report.Timestamps = timestamps

	for i, f := range report.Self {
		if i >= seriesTopN {
			break
		}
		s := HotspotSeries{Name: f.Name, Values: make([]int64, len(timestamps))}
		for j, ts := range timestamps {
			s.Values[j] = perSnapshotLeaf[ts][f.Name]
		}
		report.Series = append(report.Series, s)
	}
	return report, nil
}

func rankFrames(selfW, totalW map[string]int64, grand int64, topN int, bySelf bool) []HotspotFrame {
	src := totalW
	if bySelf {
		src = selfW
	}
	out := make([]HotspotFrame, 0, len(src))
	for name := range src {
		f := HotspotFrame{Name: name, Self: selfW[name], Total: totalW[name]}
		if grand > 0 {
			f.SelfPct = float64(f.Self) / float64(grand) * 100
			f.TotalPct = float64(f.Total) / float64(grand) * 100
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Self, out[j].Self
		if !bySelf {
			a, b = out[i].Total, out[j].Total
		}
		if a != b {
			return a > b
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > topN {
		out = out[:topN]
	}
	return out
}

// buildFlame merges folded stacks into a prefix tree rooted at "all"
func buildFlame(stackWeight map[string]int64, grand int64) *FlameNode {
	root := &FlameNode{Name: "all", Value: grand}
	index := map[*FlameNode]map[string]*FlameNode{root: {}}

	for stack, w := range stackWeight {
		node := root
		for _, frame := range strings.Split(stack, ";") {
			children := index[node]
			child, ok := children[frame]
			if !ok {
				child = &FlameNode{Name: frame}
				children[frame] = child
				index[child] = map[string]*FlameNode{}
				node.Children = append(node.Children, child)
			}
			child.Value += w
			node = child
		}
	}

	minValue := int64(float64(grand) * flameMinShare)
	pruneFlame(root, minValue)
	return root
}

// pruneFlame drops insignificant children and orders siblings by weight
func pruneFlame(n *FlameNode, minValue int64) {
	kept := n.Children[:0]
	for _, c := range n.Children {
		if c.Value >= minValue {
			kept = append(kept, c)
		}
	}
	n.Children = kept
	sort.Slice(n.Children, func(i, j int) bool {
		if n.Children[i].Value != n.Children[j].Value {
			return n.Children[i].Value > n.Children[j].Value
		}
		return n.Children[i].Name < n.Children[j].Name
	})
	for _, c := range n.Children {
		pruneFlame(c, minValue)
	}
	if len(n.Children) == 0 {
		n.Children = nil
	}
}
