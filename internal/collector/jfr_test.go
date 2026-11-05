package collector

import (
	"strings"
	"testing"
)

const sampleJstack = `2026-11-04 11:00:11
Full thread dump OpenJDK 64-Bit Server VM (17.0.13+11 mixed mode, sharing):

"cpu-burner" #20 daemon prio=5 os_prio=31 cpu=1000ms elapsed=10s tid=0x1 nid=0x6a03 runnable  [0x000000016c]
   java.lang.Thread.State: RUNNABLE
	at Burn.hashWork(Burn.java:5)
	at Burn.lambda$main$0(Burn.java:8)
	at Burn$$Lambda$14/0x0000000800c03000.run(Unknown Source)
	at java.lang.Thread.run(java.base@17.0.13/Thread.java:840)

"io-reader" #21 daemon prio=5 os_prio=31 cpu=1ms elapsed=10s tid=0x2 nid=0x6b03 runnable  [0x000000016d]
   java.lang.Thread.State: RUNNABLE
	at java.net.SocketInputStream.socketRead0(java.base@17.0.13/Native Method)
	at java.net.SocketInputStream.read(java.base@17.0.13/SocketInputStream.java:168)
	at java.lang.Thread.run(java.base@17.0.13/Thread.java:840)

"idle" #22 daemon prio=5 os_prio=31 cpu=0ms elapsed=10s tid=0x3 nid=0x6c03 waiting on condition  [0x000000016e]
   java.lang.Thread.State: TIMED_WAITING (sleeping)
	at java.lang.Thread.sleep(java.base@17.0.13/Native Method)
	at Burn.main(Burn.java:12)

"cpu-burner-2" #23 daemon prio=5 os_prio=31 cpu=1000ms elapsed=10s tid=0x4 nid=0x6d03 runnable  [0x000000016f]
   java.lang.Thread.State: RUNNABLE
	at Burn.hashWork(Burn.java:5)
	at Burn.lambda$main$0(Burn.java:8)
	at Burn$$Lambda$14/0x0000000800c03000.run(Unknown Source)
	at java.lang.Thread.run(java.base@17.0.13/Thread.java:840)
`

func TestFoldJstackThreads(t *testing.T) {
	threads := parseJstack(sampleJstack)
	if len(threads) != 4 {
		t.Fatalf("expected 4 threads, got %d", len(threads))
	}
	samples := FoldJstackThreads(threads, 48)

	// io-reader (blocked in socketRead0) and idle (TIMED_WAITING) must be filtered out;
	// the two identical cpu-burner stacks must fold into one sample with count 2.
	if len(samples) != 1 {
		t.Fatalf("expected 1 folded stack, got %d: %+v", len(samples), samples)
	}
	s := samples[0]
	if s.Kind != HotspotKindCPU || s.Count != 2 || s.Weight != 2 {
		t.Errorf("unexpected sample %+v", s)
	}
	want := "java.lang.Thread.run;Burn$$Lambda$14/0x0000000800c03000.run;Burn.lambda$main$0;Burn.hashWork"
	// module prefix "java.base@17.0.13/" must be stripped, frames root-first
	if !strings.HasPrefix(s.Stack, "java.lang.Thread.run;") || !strings.HasSuffix(s.Stack, ";Burn.hashWork") {
		t.Errorf("stack = %q, want %q", s.Stack, want)
	}
}

func TestJvmClassName(t *testing.T) {
	cases := map[string]string{
		"java/util/HashMap":      "java.util.HashMap",
		"[B":                     "byte[]",
		"[[I":                    "int[][]",
		"[Ljava/lang/String;":    "java.lang.String[]",
		"java/util/HashMap$Node": "java.util.HashMap$Node",
	}
	for in, want := range cases {
		if got := jvmClassName(in); got != want {
			t.Errorf("jvmClassName(%q) = %q, want %q", in, got, want)
		}
	}
}
