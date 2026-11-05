package storage

import (
	"testing"
	"time"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	db, err := Open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestHotspotSettingsRoundTrip(t *testing.T) {
	db := openTestDB(t)

	s, err := db.GetHotspotSetting(42)
	if err != nil || s.Enabled || s.Mode != "" {
		t.Fatalf("expected disabled default, got %+v err=%v", s, err)
	}
	if err := db.SetHotspotEnabled(42, true); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateHotspotMode(42, "jfr", "ok"); err != nil {
		t.Fatal(err)
	}
	s, _ = db.GetHotspotSetting(42)
	if !s.Enabled || s.Mode != "jfr" || s.Detail != "ok" {
		t.Fatalf("unexpected %+v", s)
	}
	pids, _ := db.ListHotspotEnabledPIDs()
	if !pids[42] {
		t.Fatalf("pid 42 should be enabled: %v", pids)
	}
	// Re-enabling (or disabling) resets the resolved mode so the daemon re-detects it
	if err := db.SetHotspotEnabled(42, false); err != nil {
		t.Fatal(err)
	}
	s, _ = db.GetHotspotSetting(42)
	if s.Enabled || s.Mode != "" {
		t.Fatalf("expected disabled with cleared mode, got %+v", s)
	}
}

func TestHotspotHourlyAggregation(t *testing.T) {
	db := openTestDB(t)
	pid := 7

	now := time.Now()
	hourStart := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, now.Location())
	prevHour := hourStart.Add(-time.Hour)

	// Two dense snapshots inside the previous hour plus one outside the window
	dense := []HotspotStackRow{
		{Kind: "cpu", Stack: "a;b;c", Count: 3, Weight: 3},
		{Kind: "cpu", Stack: "a;b;d", Count: 1, Weight: 1},
		{Kind: "alloc", Stack: "a;b;new byte[]", Count: 2, Weight: 2048},
	}
	if err := db.SaveHotspotStacks(pid, HotspotModeDense, dense, prevHour.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveHotspotStacks(pid, HotspotModeDense, dense[:1], prevHour.Add(40*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveHotspotStacks(pid, HotspotModeDense, dense[:1], prevHour.Add(-5*time.Minute)); err != nil {
		t.Fatal(err)
	}

	pids, err := db.ListHotspotDensePIDs(prevHour, hourStart)
	if err != nil || len(pids) != 1 || pids[0] != pid {
		t.Fatalf("ListHotspotDensePIDs = %v, %v", pids, err)
	}

	n, err := db.AggregateHotspotHourly(pid, prevHour, hourStart, 1) // limit 1 per kind
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("expected 2 hourly rows (1 per kind), got %d", n)
	}

	cpu, err := db.QueryHotspotStacks(pid, HotspotModeHourly, "cpu", prevHour, hourStart.Add(time.Second))
	if err != nil || len(cpu) != 1 {
		t.Fatalf("hourly cpu rows = %+v err=%v", cpu, err)
	}
	// a;b;c appeared twice in-window (3+3); the out-of-window row must be ignored
	if cpu[0].Stack != "a;b;c" || cpu[0].Count != 6 || cpu[0].Weight != 6 {
		t.Fatalf("unexpected hourly aggregate %+v", cpu[0])
	}
	if cpu[0].Timestamp != hourStart.Format(timeFmt) {
		t.Fatalf("hourly row stamped %s, want %s", cpu[0].Timestamp, hourStart.Format(timeFmt))
	}

	stats, err := db.QueryHotspotStats(pid)
	if err != nil || stats.DenseSnapshots != 3 || stats.HourlySnapshots != 1 || !stats.HasAlloc {
		t.Fatalf("stats = %+v err=%v", stats, err)
	}
}
