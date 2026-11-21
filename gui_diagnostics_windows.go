//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lxn/walk"
	"github.com/lxn/win"
)

// R005 diagnostics. The watchdog is always on and cheap: it only reads atomic
// counters every few seconds. Detailed per-call logging is gated behind
// settings.json "diagnostics": true.
const (
	guiHeartbeatStall    = 8 * time.Second
	paintHeartbeatStall  = 8 * time.Second
	workerHeartbeatStall = 35 * time.Second // one 22s warm-up plus slack
	syncSlowThreshold    = time.Second
	diagEventGap         = 10 * time.Second
)

var (
	diagVerbose  atomic.Bool
	uiConnecting atomic.Bool // GUI-written mirror for the background watchdog

	guiMsgHeartbeat   atomic.Int64
	guiPaintHeartbeat atomic.Int64
	workerHeartbeat   atomic.Int64

	syncPosts       atomic.Uint64
	syncPostMaxNS   atomic.Int64
	syncCallbacks   atomic.Uint64
	syncCallbackMax atomic.Int64
	syncSlowPosts   atomic.Uint64
	syncSlowCBs     atomic.Uint64

	paintPanicCount   atomic.Int64
	paintPanicMu      sync.Mutex
	paintPanicDetails [3]string

	lastGUIEvent     atomic.Int64
	lastPaintEvent   atomic.Int64
	lastWorkerEvent  atomic.Int64
	lastPanicEvent   atomic.Int64
	lastPanicCount   atomic.Int64
	paintStallStreak atomic.Int64

	// Injectable seam for the watchdog regression test. Production uses the
	// real GUI post and the real forced repaint below.
	postToGUI   = func(u *natUI, f func()) { u.sync(f) }
	paintRescue = forcePaintRepaint
)

// paintRescueModeResetNeeded is true for the default double-buffer mode, so
// the level-2 rescue also runs when BufferedPaint is on (R007).
func paintRescueModeResetNeeded(mode walk.PaintMode) bool {
	return mode == walk.PaintBuffered
}

// forcePaintRepaint is called on the GUI thread. It goes beyond Invalidate():
// RedrawWindow(UPDATENOW) forces WM_PAINT delivery, and on a repeated stall a
// frame-change SetWindowPos plus a PaintMode toggle resets the buffered-paint
// state that can leave the custom widget permanently blank.
func forcePaintRepaint(u *natUI, level int) bool {
	if u.cw == nil {
		return false
	}
	hwnd := u.cw.Handle()
	ok := win.RedrawWindow(hwnd, nil, 0, win.RDW_INVALIDATE|win.RDW_UPDATENOW|win.RDW_ERASE)
	_ = u.cw.Invalidate()
	if level >= 2 {
		win.SetWindowPos(hwnd, 0, 0, 0, 0, 0,
			win.SWP_NOMOVE|win.SWP_NOSIZE|win.SWP_NOZORDER|win.SWP_NOACTIVATE|win.SWP_FRAMECHANGED)
		if paintRescueModeResetNeeded(u.cw.PaintMode()) {
			// Reset the double-buffer state without changing the configured
			// default: toggle away and back while a frame is being forced.
			u.cw.SetPaintMode(walk.PaintNormal)
			u.cw.SetPaintMode(walk.PaintBuffered)
		}
	}
	return ok
}

// handlePaintStall escalates from a normal redraw to a window-position reset
// and reports whether the GUI callback actually ran. It never blocks the
// watchdog for more than two seconds.
func handlePaintStall(u *natUI, age time.Duration) bool {
	streak := paintStallStreak.Add(1)
	level := 1
	if streak >= 2 {
		level = 2
	}
	done := make(chan bool, 1)
	postToGUI(u, func() { done <- paintRescue(u, level) })
	select {
	case ok := <-done:
		if ok {
			u.diagEvent("绘制停摆补救成功：层级 %d（停摆 %.0fs，连续 %d 次）", level, age.Seconds(), streak)
		} else {
			u.diagEvent("绘制停摆补救失败：层级 %d（停摆 %.0fs）", level, age.Seconds())
		}
		return ok
	case <-time.After(2 * time.Second):
		u.diagEvent("绘制停摆补救未执行：GUI 线程 2s 内未响应")
		return false
	}
}

// atomicProgress is the non-blocking bridge from the connect worker to the
// home page. The worker stores three atomics and calls Invalidate; no GUI
// message, mutex or Synchronize queue entry is involved.
type atomicProgress struct {
	stage   atomic.Value // string
	attempt atomic.Int64
	total   atomic.Int64
}

// set publishes v and reports whether anything actually changed, so callers
// can avoid an Invalidate that would not alter a single pixel.
func (p *atomicProgress) set(v connectProgress) bool {
	oldStage, oldAttempt, oldTotal := p.get()
	changed := oldStage != v.Stage || oldAttempt != v.Attempt || oldTotal != v.Total
	p.stage.Store(v.Stage)
	p.attempt.Store(int64(v.Attempt))
	p.total.Store(int64(v.Total))
	return changed
}

func (p *atomicProgress) get() (string, int, int) {
	s, _ := p.stage.Load().(string)
	return s, int(p.attempt.Load()), int(p.total.Load())
}

func (p *atomicProgress) clear() {
	p.stage.Store("")
	p.attempt.Store(0)
	p.total.Store(0)
}

func (u *natUI) refreshDiagFlag() {
	diagVerbose.Store(u.m.set.Diagnostics)
}

// diagLog writes straight to a file next to the portable home. When the GUI
// message loop is stuck the in-memory log panel cannot be pumped, so a
// post-mortem needs evidence that does not depend on that loop.
func diagLog(format string, args ...any) {
	dir, err := appDir()
	if err != nil {
		return
	}
	line := time.Now().Format("2006-01-02 15:04:05.000 ") + fmt.Sprintf(format, args...) + "\r\n"
	f, err := os.OpenFile(filepath.Join(dir, "diagnostics.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	_, _ = f.WriteString(line)
	_ = f.Close()
}

func (u *natUI) diagEvent(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	diagLog("%s", msg)
	u.addLog(msg, true)
}

func syncCallSite() string {
	if !diagVerbose.Load() {
		return ""
	}
	_, file, line, ok := runtime.Caller(2)
	if !ok {
		return "unknown"
	}
	return fmt.Sprintf("%s:%d", filepath.Base(file), line)
}

func maxAtomic(dst *atomic.Int64, v int64) {
	for {
		old := dst.Load()
		if old >= v || dst.CompareAndSwap(old, v) {
			return
		}
	}
}

func observeSyncPost(d time.Duration) {
	syncPosts.Add(1)
	maxAtomic(&syncPostMaxNS, int64(d))
	if d > syncSlowThreshold {
		syncSlowPosts.Add(1)
		diagLog("Synchronize 入队耗时过长: %s", d)
	}
}

func observeGUICallback(u *natUI, label string, d time.Duration) {
	syncCallbacks.Add(1)
	maxAtomic(&syncCallbackMax, int64(d))
	if d <= syncSlowThreshold {
		return
	}
	syncSlowCBs.Add(1)
	diagLog("GUI 回调耗时过长: %s %s", label, d)
	u.addLog(fmt.Sprintf("GUI 回调耗时过长：%s %s", label, d), true)
}

func markWorkerTick() { workerHeartbeat.Store(time.Now().UnixNano()) }

func recordPaintPanic(u *natUI, rec any) {
	n := paintPanicCount.Add(1)
	text := fmt.Sprintf("%v", rec)
	paintPanicMu.Lock()
	if n <= int64(len(paintPanicDetails)) {
		paintPanicDetails[n-1] = time.Now().Format("15:04:05") + " " + text
	}
	paintPanicMu.Unlock()
	if n <= 3 {
		stage, attempt, total := u.progress.get()
		u.diagEvent("绘制异常 #%d（stage=%s 组合=%d/%d 连接中=%v）：%s", n, stage, attempt, total, u.m.connecting, text)
	}
}

func paintPanicSnapshot() (int64, []string) {
	paintPanicMu.Lock()
	defer paintPanicMu.Unlock()
	out := make([]string, 0, len(paintPanicDetails))
	for _, s := range paintPanicDetails {
		if s != "" {
			out = append(out, s)
		}
	}
	return paintPanicCount.Load(), out
}

func (u *natUI) diagnosticSummary() string {
	now := time.Now()
	age := func(v *atomic.Int64) string {
		x := v.Load()
		if x == 0 {
			return "未记录"
		}
		return now.Sub(time.Unix(0, x)).Round(time.Millisecond).String()
	}
	count, details := paintPanicSnapshot()
	var b strings.Builder
	fmt.Fprintf(&b, "diagnostics: gui_msg=%s paint=%s worker=%s\r\n", age(&guiMsgHeartbeat), age(&guiPaintHeartbeat), age(&workerHeartbeat))
	fmt.Fprintf(&b, "sync: posts=%d max=%s callbacks=%d max=%s slow_posts=%d slow_callbacks=%d\r\n",
		syncPosts.Load(), time.Duration(syncPostMaxNS.Load()), syncCallbacks.Load(), time.Duration(syncCallbackMax.Load()),
		syncSlowPosts.Load(), syncSlowCBs.Load())
	fmt.Fprintf(&b, "paint panics: %d\r\n", count)
	for _, d := range details {
		b.WriteString("  " + d + "\r\n")
	}
	return b.String()
}

func (u *natUI) startDiagnostics() {
	u.refreshDiagFlag()
	now := time.Now().UnixNano()
	guiMsgHeartbeat.Store(now)
	guiPaintHeartbeat.Store(now)
	workerHeartbeat.Store(now)
	go u.guiHeartbeatLoop()
	go u.watchdogLoop()
}

// guiHeartbeatLoop posts a no-op callback into the GUI message queue. If the
// timestamp stops advancing, the GUI thread is not draining its queue.
func (u *natUI) guiHeartbeatLoop() {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-u.ctx.Done():
			return
		case <-t.C:
		}
		at := time.Now().UnixNano()
		u.sync(func() { guiMsgHeartbeat.Store(at) })
	}
}

func (u *natUI) watchdogLoop() {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-u.ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now()
		msgAge := time.Duration(now.UnixNano() - guiMsgHeartbeat.Load())
		paintAge := time.Duration(now.UnixNano() - guiPaintHeartbeat.Load())
		workerAge := time.Duration(now.UnixNano() - workerHeartbeat.Load())
		if msgAge > guiHeartbeatStall && time.Since(time.Unix(0, lastGUIEvent.Load())) > diagEventGap {
			lastGUIEvent.Store(now.UnixNano())
			u.diagEvent("GUI 消息循环停摆：Synchronize 心跳 %.0fs 未执行", msgAge.Seconds())
		}
		if msgAge <= guiHeartbeatStall && paintAge > paintHeartbeatStall && time.Since(time.Unix(0, lastPaintEvent.Load())) > diagEventGap {
			lastPaintEvent.Store(now.UnixNano())
			u.diagEvent("绘制停摆：消息循环仍在，但 %.0fs 未绘制", paintAge.Seconds())
			handlePaintStall(u, paintAge)
		}
		if workerAge > workerHeartbeatStall && time.Since(time.Unix(0, lastWorkerEvent.Load())) > diagEventGap {
			lastWorkerEvent.Store(now.UnixNano())
			u.diagEvent("后台循环停摆：%.0fs 无心跳（连接中=%v）", workerAge.Seconds(), uiConnecting.Load())
		}
		if n := paintPanicCount.Load(); n > 0 && n != lastPanicCount.Load() && time.Since(time.Unix(0, lastPanicEvent.Load())) > diagEventGap {
			lastPanicCount.Store(n)
			lastPanicEvent.Store(now.UnixNano())
			u.diagEvent("绘制异常累计 %d 次；stage=%s", n, func() string { s, _, _ := u.progress.get(); return s }())
		}
		if diagVerbose.Load() {
			diagLog("heartbeat gui=%.1fs paint=%.1fs worker=%.1fs sync_posts=%d slow_cb=%d",
				msgAge.Seconds(), paintAge.Seconds(), workerAge.Seconds(), syncPosts.Load(), syncSlowCBs.Load())
		}
	}
}
