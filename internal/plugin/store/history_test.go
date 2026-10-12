package store

import (
	"testing"
)

func TestHistoryKeepsThirtyDaysAndCountsARetriedDayOnce(t *testing.T) {
	dir := t.TempDir()
	h, err := OpenHistory(dir)
	if err != nil {
		t.Fatal(err)
	}
	add := func(date string, up, down uint64) {
		t.Helper()
		if err := h.Add([]Rolled{{Server: 1, User: "alice", Day: Day{Date: date, Up: up, Down: down}}}, "2026-10-11"); err != nil {
			t.Fatal(err)
		}
	}
	add("2026-09-10", 1000, 1000) // 31 days back: outside the window
	add("2026-09-12", 1, 2)
	add("2026-10-10", 10, 20)
	add("2026-10-10", 10, 20) // the same day written again after a crash
	if up, down := h.Sum(1, "alice", FirstDay("2026-10-11")); up != 11 || down != 22 {
		t.Fatalf("sum = %d/%d, want 11/22", up, down)
	}

	// it survives a restart
	again, err := OpenHistory(dir)
	if err != nil {
		t.Fatal(err)
	}
	if up, _ := again.Sum(1, "alice", "2026-10-01"); up != 10 {
		t.Fatalf("after reopening: %d", up)
	}
	again.Forget(1, "alice")
	if up, _ := again.Sum(1, "alice", "2000-01-01"); up != 0 {
		t.Fatal("a forgotten user still has history")
	}
	if FirstDay("2026-10-11") != "2026-09-12" {
		t.Errorf("FirstDay = %s", FirstDay("2026-10-11"))
	}
}
