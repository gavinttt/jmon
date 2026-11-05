package collector

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// HotspotRecordingName is the JFR recording name jmon owns inside the target JVM
const HotspotRecordingName = "jmon-hotspot"

// Hotspot sample kinds
const (
	HotspotKindCPU   = "cpu"   // execution samples (which Java code is running)
	HotspotKindAlloc = "alloc" // allocation samples (which code allocates memory, weight = bytes)
)

// StackSample is one folded stack trace (root first, frames joined by ';') with aggregated weight
type StackSample struct {
	Kind   string
	Stack  string
	Count  int64
	Weight int64 // cpu: == Count; alloc: estimated bytes
}

// jcmd runs a jcmd diagnostic command against pid. jcmd exits 0 even when the target
// JVM rejects the command, so the output text is also inspected for error markers.
func jcmd(pid int, args ...string) (string, error) {
	full := append([]string{fmt.Sprintf("%d", pid)}, args...)
	out, err := exec.Command("jcmd", full...).Output()
	text := string(out)
	if err != nil {
		return text, fmt.Errorf("jcmd %s failed for pid %d: %w: %s", args[0], pid, err, strings.TrimSpace(text))
	}
	for _, marker := range []string{"Unknown argument", "Exception", "Could not", "Unable to", "Illegal", "Unknown diagnostic command", "not found"} {
		if strings.Contains(text, marker) {
			return text, fmt.Errorf("jcmd %s rejected for pid %d: %s", args[0], pid, strings.TrimSpace(text))
		}
	}
	return text, nil
}

// JFRSupported reports whether the target JVM exposes the JFR.start diagnostic command
// (JDK 11+, or OpenJDK 8u262+ where JFR was backported).
func JFRSupported(pid int) bool {
	out, err := exec.Command("jcmd", fmt.Sprintf("%d", pid), "help").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "JFR.start")
}

// JFRRecordingActive reports whether a recording with the given name is already running
func JFRRecordingActive(pid int, name string) bool {
	out, err := jcmd(pid, "JFR.check")
	if err != nil {
		return false
	}
	return strings.Contains(out, "name="+name+" ")
}

// StartJFR starts a continuous, disk-backed recording limited to the last 10 minutes.
// Starting a recording never triggers a GC; the profile settings sample Java execution
// every 10ms and throttle allocation samples.
func StartJFR(pid int, name string) error {
	_, err := jcmd(pid, "JFR.start", "name="+name, "settings=profile", "disk=true", "maxage=10m")
	return err
}

// StopJFR stops and discards the named recording
func StopJFR(pid int, name string) error {
	_, err := jcmd(pid, "JFR.stop", "name="+name)
	return err
}

// DumpJFR copies recording data to file. It first tries to limit the dump to the last
// `since` duration (begin=-Ns, JDK 11+); older JVMs that reject the option fall back to a
// full dump, which the caller trims by event timestamp.
func DumpJFR(pid int, name, file string, since time.Duration) error {
	secs := int(since.Seconds())
	if secs < 1 {
		secs = 1
	}
	_, err := jcmd(pid, "JFR.dump", "name="+name, "filename="+file, fmt.Sprintf("begin=-%ds", secs))
	if err == nil {
		return nil
	}
	if !strings.Contains(err.Error(), "Unknown argument") {
		return err
	}
	_, err = jcmd(pid, "JFR.dump", "name="+name, "filename="+file)
	return err
}

// JavaHome returns the java.home of the target JVM (empty if unavailable)
func JavaHome(pid int) string {
	out, err := exec.Command("jcmd", fmt.Sprintf("%d", pid), "VM.system_properties").Output()
	if err != nil {
		return ""
	}
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "java.home=") {
			v := strings.TrimPrefix(line, "java.home=")
			// jcmd escapes ':' and '\' in property values (Windows paths)
			v = strings.ReplaceAll(v, `\:`, ":")
			v = strings.ReplaceAll(v, `\\`, `\`)
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// FindJfrTool locates a `jfr` binary able to parse recordings: the target JVM's own JDK
// first, then PATH, then the user-configured path. Returns "" when none is available
// (typical for JDK 8 targets without any JDK 11+ installed).
func FindJfrTool(pid int, configured string) string {
	if home := JavaHome(pid); home != "" {
		for _, name := range []string{"jfr", "jfr.exe"} {
			p := filepath.Join(home, "bin", name)
			if _, err := exec.LookPath(p); err == nil {
				return p
			}
		}
	}
	if p, err := exec.LookPath("jfr"); err == nil {
		return p
	}
	if configured != "" {
		if p, err := exec.LookPath(configured); err == nil {
			return p
		}
	}
	return ""
}

// --- JFR JSON parsing ---

type jfrFrame struct {
	Method struct {
		Type struct {
			Name string `json:"name"`
		} `json:"type"`
		Name   string `json:"name"`
		Hidden bool   `json:"hidden"`
	} `json:"method"`
}

type jfrEvent struct {
	Type   string `json:"type"`
	Values struct {
		StartTime  string `json:"startTime"`
		StackTrace *struct {
			Frames []jfrFrame `json:"frames"`
		} `json:"stackTrace"`
		Weight         int64 `json:"weight"`         // jdk.ObjectAllocationSample (JDK 16+)
		AllocationSize int64 `json:"allocationSize"` // jdk.ObjectAllocationOutsideTLAB
		TlabSize       int64 `json:"tlabSize"`       // jdk.ObjectAllocationInNewTLAB
		ObjectClass    *struct {
			Name string `json:"name"`
		} `json:"objectClass"`
	} `json:"values"`
}

var jfrHotspotEvents = strings.Join([]string{
	"jdk.ExecutionSample",
	"jdk.ObjectAllocationSample",
	"jdk.ObjectAllocationInNewTLAB",
	"jdk.ObjectAllocationOutsideTLAB",
}, ",")

// ParseJFRHotspots streams `jfr print --json` output for the given recording file and
// folds execution/allocation samples newer than `since` into aggregated stacks.
// Output is sorted by weight descending.
func ParseJFRHotspots(jfrTool, file string, since time.Time, depth int) ([]StackSample, error) {
	cmd := exec.Command(jfrTool, "print", "--json",
		"--events", jfrHotspotEvents,
		"--stack-depth", fmt.Sprintf("%d", depth),
		file)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to run jfr print: %w", err)
	}

	agg := make(map[string]*StackSample)
	dec := json.NewDecoder(bufio.NewReaderSize(stdout, 1<<20))

	// Advance to the "events" array
	found := false
	for !found {
		tok, err := dec.Token()
		if err != nil {
			cmd.Wait()
			return nil, fmt.Errorf("jfr output has no events array: %w", err)
		}
		if s, ok := tok.(string); ok && s == "events" {
			next, err := dec.Token()
			if err != nil {
				cmd.Wait()
				return nil, err
			}
			if d, ok := next.(json.Delim); ok && d == '[' {
				found = true
			}
		}
	}

	for dec.More() {
		var ev jfrEvent
		if err := dec.Decode(&ev); err != nil {
			cmd.Process.Kill()
			cmd.Wait()
			return nil, fmt.Errorf("jfr json decode: %w", err)
		}
		if ev.Values.StackTrace == nil || len(ev.Values.StackTrace.Frames) == 0 {
			continue
		}
		if !since.IsZero() {
			if t, err := time.Parse(time.RFC3339Nano, ev.Values.StartTime); err == nil && !t.After(since) {
				continue
			}
		}

		kind := HotspotKindCPU
		var weight int64 = 1
		switch ev.Type {
		case "jdk.ObjectAllocationSample":
			kind, weight = HotspotKindAlloc, ev.Values.Weight
		case "jdk.ObjectAllocationInNewTLAB":
			kind, weight = HotspotKindAlloc, ev.Values.TlabSize
		case "jdk.ObjectAllocationOutsideTLAB":
			kind, weight = HotspotKindAlloc, ev.Values.AllocationSize
		}
		if weight <= 0 {
			weight = 1
		}

		frames := ev.Values.StackTrace.Frames
		names := make([]string, 0, len(frames)+1)
		// JFR lists the top frame first; folded stacks are root first
		for i := len(frames) - 1; i >= 0; i-- {
			f := frames[i]
			if f.Method.Hidden {
				continue
			}
			names = append(names, jvmClassName(f.Method.Type.Name)+"."+f.Method.Name)
		}
		if kind == HotspotKindAlloc && ev.Values.ObjectClass != nil {
			names = append(names, "new "+jvmClassName(ev.Values.ObjectClass.Name))
		}
		if len(names) == 0 {
			continue
		}
		addSample(agg, kind, strings.Join(names, ";"), 1, weight)
	}
	// Drain the remainder so the child exits cleanly
	for {
		if _, err := dec.Token(); err != nil {
			break
		}
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("jfr print failed: %w", err)
	}
	return sortedSamples(agg), nil
}

// --- jstack-based fallback ---

// Top frames that mean "RUNNABLE but really blocked in native I/O"; jstack reports these
// threads as RUNNABLE so they must be filtered out of CPU hotspot sampling.
var blockingNativeFrames = map[string]bool{
	"socketRead0": true, "socketAccept": true, "accept0": true, "read0": true, "readBytes": true,
	"receive0": true, "poll": true, "poll0": true, "epollWait": true, "kevent": true, "kqueue": true,
	"select": true, "select0": true, "wait0": true, "park": true, "sleep0": true,
	"waitForProcessExit0": true, "waitForReferencePendingList": true, "socketConnect": true,
	"connect0": true, "write0": true, "socketWrite0": true,
}

var jstackFrameRe = regexp.MustCompile(`^\s*at\s+(?:[\w.]+(?:@[\w.+-]+)?/)?([\w.$<>]+)\(`)

// FoldJstackThreads turns a jstack snapshot into CPU-kind stack samples: every RUNNABLE
// Java thread contributes one sample. Precision is far below JFR (one sample per collection
// interval and safepoint-biased) but works on any JDK without JFR.
func FoldJstackThreads(threads []ThreadInfo, depth int) []StackSample {
	agg := make(map[string]*StackSample)
	for _, t := range threads {
		if t.State != "RUNNABLE" || t.StackTrace == "" {
			continue
		}
		var frames []string // top first
		scanner := bufio.NewScanner(strings.NewReader(t.StackTrace))
		for scanner.Scan() {
			m := jstackFrameRe.FindStringSubmatch(scanner.Text())
			if m == nil {
				continue
			}
			frames = append(frames, m[1])
			if len(frames) >= depth {
				break
			}
		}
		if len(frames) == 0 {
			continue
		}
		top := frames[0]
		if blockingNativeFrames[top[strings.LastIndex(top, ".")+1:]] {
			continue
		}
		names := make([]string, len(frames))
		for i, f := range frames {
			names[len(frames)-1-i] = f
		}
		addSample(agg, HotspotKindCPU, strings.Join(names, ";"), 1, 1)
	}
	return sortedSamples(agg)
}

// --- helpers ---

func addSample(agg map[string]*StackSample, kind, stack string, count, weight int64) {
	key := kind + "\x00" + stack
	if s, ok := agg[key]; ok {
		s.Count += count
		s.Weight += weight
		return
	}
	agg[key] = &StackSample{Kind: kind, Stack: stack, Count: count, Weight: weight}
}

func sortedSamples(agg map[string]*StackSample) []StackSample {
	out := make([]StackSample, 0, len(agg))
	for _, s := range agg {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Weight != out[j].Weight {
			return out[i].Weight > out[j].Weight
		}
		return out[i].Stack < out[j].Stack
	})
	return out
}

// jvmClassName converts internal names ("java/util/HashMap", "[B", "[Ljava/lang/String;")
// into Java source notation.
func jvmClassName(n string) string {
	dims := 0
	for strings.HasPrefix(n, "[") {
		dims++
		n = n[1:]
	}
	if dims > 0 {
		switch {
		case n == "B":
			n = "byte"
		case n == "C":
			n = "char"
		case n == "I":
			n = "int"
		case n == "J":
			n = "long"
		case n == "S":
			n = "short"
		case n == "Z":
			n = "boolean"
		case n == "F":
			n = "float"
		case n == "D":
			n = "double"
		case strings.HasPrefix(n, "L") && strings.HasSuffix(n, ";"):
			n = n[1 : len(n)-1]
		}
	}
	n = strings.ReplaceAll(n, "/", ".")
	return n + strings.Repeat("[]", dims)
}
