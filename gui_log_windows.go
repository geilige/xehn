//go:build windows

package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The log panel used to receive one Synchronize() per core line. A dead OpenVPN
// relay prints hundreds of "dial ... context deadline exceeded" warnings in
// twenty seconds; each of those messages became its own GUI message, and the
// window stopped answering while the queue drained. Logs now go through one
// bounded queue, are aggregated by signature, and reach the GUI in 10 Hz
// batches from a single consumer.
const (
	guiLogQueueSize    = 2048
	guiLogMaxLines     = 2000
	guiLogFlushEvery   = 100 * time.Millisecond
	guiLogAggregateFor = 3 * time.Second
)

// openvpnWarmupActive is set by the connect pipeline while the chained relay
// is being warmed up. Timeouts in that window are expected (the first inbound
// dials always lose the race with the outbound handshake) and must never be
// printed as warnings.
var openvpnWarmupActive atomic.Bool

type logRecord struct {
	at    time.Time
	text  string
	lv    int // 0 = info, 1 = warning, 2 = error
	core  bool
	stage bool
}

type logAggregate struct {
	first       logRecord
	last        time.Time
	count       int
	warmupQuiet bool
}

// logPipeline is the producer/consumer bridge between arbitrary worker
// goroutines and the GUI thread. Only the consumer touches u.m.logs, and it
// does so inside u.sync, so painting still sees a single-threaded model.
type logPipeline struct {
	u      *natUI
	in     chan logRecord
	closed chan struct{}
	once   sync.Once

	mu      sync.Mutex
	pending []natLog
	aggs    map[string]*logAggregate
}

func startLogPipeline(u *natUI) *logPipeline {
	p := &logPipeline{
		u:      u,
		in:     make(chan logRecord, guiLogQueueSize),
		closed: make(chan struct{}),
		aggs:   make(map[string]*logAggregate),
	}
	go p.loop()
	return p
}

func (p *logPipeline) close() {
	p.once.Do(func() { close(p.closed) })
}

// enqueue never blocks a producer. When the queue is full the oldest record is
// discarded so the newest (and therefore the most relevant) line survives.
func (p *logPipeline) enqueue(r logRecord) {
	if r.at.IsZero() {
		r.at = time.Now()
	}
	select {
	case p.in <- r:
		return
	default:
	}
	select {
	case <-p.in:
	default:
	}
	select {
	case p.in <- r:
	default:
	}
}

func (p *logPipeline) loop() {
	t := time.NewTicker(guiLogFlushEvery)
	defer t.Stop()
	for {
		select {
		case r := <-p.in:
			p.accept(r)
		case now := <-t.C:
			p.flush(now)
		case <-p.closed:
			p.flush(time.Now())
			return
		}
	}
}

// logSignature groups lines that differ only in endpoint, port or count. An
// empty signature means "pass this line through unchanged".
func logSignature(r logRecord) string {
	low := strings.ToLower(r.text)
	switch {
	case strings.Contains(low, "dial openvpn") && strings.Contains(low, "context deadline exceeded"):
		return "openvpn-dial-timeout"
	case strings.Contains(low, "start tun interface timeout"):
		return "tun-start-timeout"
	case r.core && strings.Contains(low, "using openvpn") && strings.Contains(low, "openvpn exit"):
		// A live relay prints one "match ... using OPENVPN[OpenVPN Exit]"
		// line per connection; left raw it buries the stage messages.
		return "core-using-openvpn"
	case r.core && strings.Contains(low, "match") && strings.Contains(low, "using "):
		return "core-match-using"
	case r.lv == 1 && strings.Contains(low, "context deadline exceeded"):
		return "generic-dial-timeout"
	}
	return ""
}

func (p *logPipeline) accept(r logRecord) {
	p.mu.Lock()
	defer p.mu.Unlock()

	sig := logSignature(r)
	if sig != "" && openvpnWarmupActive.Load() {
		// Expected warm-up failures: count them, then report one INFO line
		// instead of a wall of WARNs.
		a := p.aggs["warmup-quiet"]
		if a == nil {
			a = &logAggregate{first: r, warmupQuiet: true}
			p.aggs["warmup-quiet"] = a
		}
		a.count++
		a.last = r.at
		return
	}
	if sig == "" {
		p.pending = append(p.pending, natLog{at: r.at, text: r.text, lv: r.lv, core: r.core, stage: r.stage})
		return
	}
	a := p.aggs[sig]
	if a != nil && r.at.Sub(a.first.at) < guiLogAggregateFor {
		a.count++
		a.last = r.at
		return
	}
	if a != nil {
		// A new signature arrived after the window: emit the previous group
		// with a display timestamp of "now", not of its last member, so it can
		// never appear after a newer line from an earlier batch.
		p.emitAggregate(a, time.Now())
		delete(p.aggs, sig)
	}
	p.aggs[sig] = &logAggregate{first: r, last: r.at, count: 1}
}

func (p *logPipeline) emitAggregate(a *logAggregate, now time.Time) {
	text := a.first.text
	lv := a.first.lv
	if a.warmupQuiet {
		lv = 0
		text = trf("预热期已抑制 %d 条预期超时", a.count)
	} else {
		secs := a.last.Sub(a.first.at).Seconds()
		if secs < 0 {
			secs = 0
		}
		text = fmt.Sprintf("%s  ×%d in %.0fs", a.first.text, a.count, secs)
	}
	p.pending = append(p.pending, natLog{
		at: now, text: text, lv: lv, core: a.first.core, stage: a.first.stage,
	})
}

// flush is also called directly by tests with a synthetic "now" so an
// aggregation window can elapse without sleeping.
func (p *logPipeline) flush(now time.Time) {
	p.mu.Lock()
	for k, a := range p.aggs {
		if !now.Before(a.first.at.Add(guiLogAggregateFor)) {
			p.emitAggregate(a, now)
			delete(p.aggs, k)
		}
	}
	batch := p.pending
	p.pending = nil
	p.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	// An aggregate emitted at the end of its window carries the last
	// member's timestamp, which can be older than lines accepted in the
	// meantime. Sort by timestamp so the panel never shows time going
	// backwards.
	sort.SliceStable(batch, func(i, j int) bool { return batch[i].at.Before(batch[j].at) })
	apply := func() {
		p.u.m.logs = append(p.u.m.logs, batch...)
		if len(p.u.m.logs) > guiLogMaxLines {
			extra := len(p.u.m.logs) - guiLogMaxLines
			copy(p.u.m.logs, p.u.m.logs[extra:])
			p.u.m.logs = p.u.m.logs[:guiLogMaxLines]
		}
		p.u.invalidate()
	}
	if p.u.cw == nil {
		// Headless tests (and the window's own construction phase) have no
		// message loop to post into.
		apply()
		return
	}
	p.u.sync(apply)
}
