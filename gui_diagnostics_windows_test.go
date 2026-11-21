//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lxn/walk"
)

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

func TestPaintRescueResetsBufferedMode(t *testing.T) {
	if !paintRescueModeResetNeeded(walk.PaintBuffered) {
		t.Fatal("level-2 rescue must reset the default buffered paint mode")
	}
	if paintRescueModeResetNeeded(walk.PaintNormal) {
		t.Fatal("PaintNormal needs no buffer reset")
	}
}

func TestHandlePaintStallTriggersRescue(t *testing.T) {
	t.Setenv("WARP_CLIENT_HOME", t.TempDir())
	oldPost, oldRescue := postToGUI, paintRescue
	t.Cleanup(func() { postToGUI, paintRescue = oldPost, oldRescue })

	var levels []int
	postToGUI = func(_ *natUI, f func()) { f() }
	paintRescue = func(_ *natUI, level int) bool {
		levels = append(levels, level)
		return true
	}
	u := &natUI{m: newNatModel()}

	if !handlePaintStall(u, 9*time.Second) {
		t.Fatal("first rescue did not report success")
	}
	if !handlePaintStall(u, 9*time.Second) {
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
	for _, want := range []string{"diagnostics:", "sync:", "paint panics:"} {
		if !strings.Contains(s, want) {
			t.Fatalf("summary missing %q: %s", want, s)
		}
	}
}
