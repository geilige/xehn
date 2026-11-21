//go:build windows

package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestLogPipelineAggregatesDeadlineWarnings(t *testing.T) {
	u := &natUI{m: newNatModel()}
	p := startLogPipeline(u)
	defer p.close()

	base := time.Now()
	for i := 0; i < 187; i++ {
		p.accept(logRecord{
			at:   base.Add(time.Duration(i) * 10 * time.Millisecond),
			text: "[WARN] dial OPENVPN error: context deadline exceeded",
			lv:   1,
			core: true,
		})
	}
	p.flush(base.Add(4 * time.Second))

	if len(u.m.logs) != 1 {
		t.Fatalf("lines = %d, want 1 aggregated line", len(u.m.logs))
	}
	got := u.m.logs[0].text
	if !strings.Contains(got, "×187") || !strings.Contains(got, "in 2s") {
		t.Fatalf("aggregated line = %q", got)
	}
}

func TestLogPipelineSuppressesWarmupTimeouts(t *testing.T) {
	u := &natUI{m: newNatModel()}
	p := startLogPipeline(u)
	defer p.close()

	openvpnWarmupActive.Store(true)
	defer openvpnWarmupActive.Store(false)
	base := time.Now()
	for i := 0; i < 40; i++ {
		p.accept(logRecord{
			at:   base.Add(time.Duration(i) * time.Second),
			text: "dial OPENVPN error: context deadline exceeded",
			lv:   1,
		})
	}
	p.flush(base.Add(2 * time.Minute))

	if len(u.m.logs) != 1 {
		t.Fatalf("lines = %d, want 1 summary", len(u.m.logs))
	}
	if u.m.logs[0].lv != 0 {
		t.Fatalf("summary level = %d, want info", u.m.logs[0].lv)
	}
	if !strings.Contains(u.m.logs[0].text, "40") {
		t.Fatalf("summary = %q, want the suppressed count", u.m.logs[0].text)
	}
}

func TestLogPipelineAggregatesCoreMatchNoise(t *testing.T) {
	u := &natUI{m: newNatModel()}
	p := startLogPipeline(u)
	defer p.close()

	base := time.Now()
	for i := 0; i < 90; i++ {
		p.accept(logRecord{
			at:   base.Add(time.Duration(i) * 10 * time.Millisecond),
			text: fmt.Sprintf("[INFO] [TCP] match Rule[OPENVPN] using OPENVPN[OpenVPN Exit] connection %d", i),
			core: true,
		})
	}
	p.flush(base.Add(4 * time.Second))
	if len(u.m.logs) != 1 {
		t.Fatalf("lines = %d, want 1 aggregated CORE line", len(u.m.logs))
	}
	if !strings.Contains(u.m.logs[0].text, "×90") {
		t.Fatalf("CORE line = %q", u.m.logs[0].text)
	}
}

func TestLogPipelineFlushKeepsTimestampsMonotonic(t *testing.T) {
	u := &natUI{m: newNatModel()}
	p := startLogPipeline(u)
	defer p.close()

	base := time.Now()
	// A warning whose last member is older than a later normal line. Its
	// aggregate is emitted at flush time, after the normal line was accepted.
	for i := 0; i < 3; i++ {
		p.accept(logRecord{
			at:   base.Add(time.Duration(i) * time.Second),
			text: "dial OPENVPN error: context deadline exceeded",
			lv:   1,
		})
	}
	p.accept(logRecord{at: base.Add(5 * time.Second), text: "later line"})
	p.flush(base.Add(4 * time.Second))

	if len(u.m.logs) != 2 {
		t.Fatalf("lines = %d, want 2", len(u.m.logs))
	}
	if u.m.logs[0].at.After(u.m.logs[1].at) {
		t.Fatalf("timestamps went backwards: %s then %s", u.m.logs[0].at, u.m.logs[1].at)
	}
}

// TestLogPipelineCrossBatchTimestampsMonotonic covers the gap the old test
// missed: an aggregate emitted in a LATER batch used its last member's time,
// which is older than a normal line already shown in an earlier batch.
func TestLogPipelineCrossBatchTimestampsMonotonic(t *testing.T) {
	u := &natUI{m: newNatModel()}
	p := startLogPipeline(u)
	defer p.close()

	base := time.Now()
	// Members are older than the normal line and stay inside one window.
	for i := 0; i < 5; i++ {
		p.accept(logRecord{
			at:   base.Add(time.Duration(i) * 500 * time.Millisecond),
			text: "[INFO] [TCP] match Match using OPENVPN[OpenVPN Exit]",
			core: true,
		})
	}
	normalAt := base.Add(2500 * time.Millisecond)
	p.accept(logRecord{at: normalAt, text: "已连接"})

	// First batch: the window has not expired, so only "已连接" is written.
	p.flush(base.Add(2500 * time.Millisecond))
	if len(u.m.logs) != 1 || u.m.logs[0].text != "已连接" {
		t.Fatalf("first batch = %+v, want only the normal line", u.m.logs)
	}

	// Second batch: aggregation expires and emits an old-timestamp line after
	// the newer normal line. Before the fix this makes time go backwards.
	p.flush(base.Add(5 * time.Second))
	if len(u.m.logs) != 2 {
		t.Fatalf("lines = %d, want 2", len(u.m.logs))
	}
	if u.m.logs[0].at.After(u.m.logs[1].at) {
		t.Fatalf("cross-batch timestamps went backwards: %s then %s",
			u.m.logs[0].at.Format(time.RFC3339Nano), u.m.logs[1].at.Format(time.RFC3339Nano))
	}
}

func TestLogPipelineCapsRetainedLines(t *testing.T) {
	u := &natUI{m: newNatModel()}
	p := startLogPipeline(u)
	defer p.close()

	base := time.Now()
	for i := 0; i < guiLogMaxLines+500; i++ {
		p.accept(logRecord{at: base, text: "line", lv: 0})
	}
	p.flush(base)

	if len(u.m.logs) != guiLogMaxLines {
		t.Fatalf("lines = %d, want %d", len(u.m.logs), guiLogMaxLines)
	}
}
