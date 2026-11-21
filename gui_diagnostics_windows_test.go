//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func resetPaintDiagnosticsForTest() {
	invalidateRequests.Store(0)
	paintExecutions.Store(0)
	lastServedRequest.Store(0)
	lastInvalidateRequest.Store(0)
	paintStallStreak.Store(0)
	nextPaintCheck.Store(0)
}

func TestAtomicProgress(t *testing.T) {
	var p atomicProgress
	if s, a, total := p.get(); s != "" || a != 0 || total != 0 {
		t.Fatalf("zero progress = %q %d/%d", s, a, total)
	}
	p.set(connectProgress{Stage: stageHandshake, Attempt: 3, Total: 6})
	if s, a, total := p.get(); s != stageHandshake || a != 3 || total != 6 {
		t.Fatalf("progress = %q %d/%d", s, a, total)
	}
	p.clear()
	if s, a, total := p.get(); s != "" || a != 0 || total != 0 {
		t.Fatalf("cleared progress = %q %d/%d", s, a, total)
	}
}

// TestIsTruePaintStall pins the R010 false positive: a quiet window with no
// outstanding repaint request must never be called "paint stalled", however
// long ago the last paint ran.
func TestIsTruePaintStall(t *testing.T) {
	now := time.Now()
	old := now.Add(-time.Hour).UnixNano()
	if isTruePaintStall(0, 0, old, now) {
		t.Fatal("a quiet window with no requests was judged stalled")
	}
	if isTruePaintStall(5, 5, old, now) {
		t.Fatal("a fully served request queue was judged stalled")
	}
	if !isTruePaintStall(6, 5, old, now) {
		t.Fatal("an unserved repaint request must count as a stall")
	}
	if isTruePaintStall(6, 5, now.Add(-time.Second).UnixNano(), now) {
		t.Fatal("a fresh request still inside the grace window must not count")
	}
}

func TestStallBackoff(t *testing.T) {
	cases := []struct {
		streak int64
		want   time.Duration
	}{
		{1, 3 * time.Second},
		{3, 12 * time.Second},
		{4, 60 * time.Second},
		{286, 60 * time.Second},
	}
	for _, tc := range cases {
		if got := stallBackoff(tc.streak); got != tc.want {
			t.Fatalf("stallBackoff(%d) = %s, want %s", tc.streak, got, tc.want)
		}
	}
}

// TestHandlePaintStallRequiresPaintExecution replaces the old test that
// trusted paintRescue's return value. A rescue that only calls Windows APIs
// without a paint callback must be reported as ineffective.
func TestHandlePaintStallRequiresPaintExecution(t *testing.T) {
	t.Setenv("WARP_CLIENT_HOME", t.TempDir())
	oldPost, oldRescue := postToGUI, paintRescue
	t.Cleanup(func() { postToGUI, paintRescue = oldPost, oldRescue })
	postToGUI = func(_ *natUI, f func()) { f() }

	u := &natUI{m: newNatModel()}
	resetPaintDiagnosticsForTest()
	paintRescue = func(_ *natUI, _ int) {} // API "succeeds" but paint never runs
	if handlePaintStall(u, 9*time.Second, 1) {
		t.Fatal("rescue without a paint execution was falsely reported successful")
	}

	resetPaintDiagnosticsForTest()
	paintRescue = func(_ *natUI, _ int) { markPaintExecuted() }
	if !handlePaintStall(u, 9*time.Second, 1) {
		t.Fatal("rescue followed by a real paint was not reported successful")
	}
}

func TestHandlePaintStallTriggersRescue(t *testing.T) {
	t.Setenv("WARP_CLIENT_HOME", t.TempDir())
	oldPost, oldRescue := postToGUI, paintRescue
	t.Cleanup(func() { postToGUI, paintRescue = oldPost, oldRescue })

	var levels []int
	postToGUI = func(_ *natUI, f func()) { f() }
	paintRescue = func(_ *natUI, level int) {
		levels = append(levels, level)
		markPaintExecuted()
	}
	u := &natUI{m: newNatModel()}
	resetPaintDiagnosticsForTest()

	if !handlePaintStall(u, 9*time.Second, 1) {
		t.Fatal("first rescue did not report success")
	}
	if !handlePaintStall(u, 9*time.Second, 2) {
		t.Fatal("second rescue did not report success")
	}
	if len(levels) != 2 || levels[0] != 1 || levels[1] != 2 {
		t.Fatalf("rescue levels = %v, want [1 2]", levels)
	}
}

func TestDiagLogWritesStandaloneFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WARP_CLIENT_HOME", dir)
	diagLog("probe %d", 42)
	data, err := os.ReadFile(filepath.Join(dir, "diagnostics.log"))
	if err != nil {
		t.Fatalf("diagnostics.log: %v", err)
	}
	if !strings.Contains(string(data), "probe 42") {
		t.Fatalf("log content = %q", data)
	}
}

func TestDiagnosticSummaryIncludesCounters(t *testing.T) {
	u := &natUI{m: newNatModel()}
	s := u.diagnosticSummary()
	for _, want := range []string{"diagnostics:", "sync:", "paint: requests="} {
		if !strings.Contains(s, want) {
			t.Fatalf("summary missing %q: %s", want, s)
		}
	}
}
