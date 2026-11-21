package main

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/xhwarp/warp-client/internal/cache"
)

const (
	// endpointFailLimit consecutive handshake failures before an endpoint is
	// parked. Failing once is common on a churning edge; twice in one session
	// is a strong signal that the cached ranking is stale.
	endpointFailLimit = 2
	// endpointFailQuarantine is deliberately short: a fresh scan can still
	// bring the endpoint back within the same working session.
	endpointFailQuarantine = 5 * time.Minute
)

// rankHealthyEndpoints puts endpoints with a clean recent record first. The
// sort is stable, so a cache whose entries all have zero failures keeps its
// original latency ranking — health is a tie-breaker, not a new ranking.
func rankHealthyEndpoints(cands []cachedEndpoint) []cachedEndpoint {
	if len(cands) < 2 {
		return cands
	}
	out := append([]cachedEndpoint(nil), cands...)
	now := time.Now()
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		aq := now.Before(a.QuarantinedUntil)
		bq := now.Before(b.QuarantinedUntil)
		if aq != bq {
			return !aq // healthy first
		}
		return a.FailCount < b.FailCount
	})
	return out
}

func mutateEndpointHealth(opts options, ce cachedEndpoint, fn func(*cachedEndpoint) bool) {
	if ce.Endpoint == "" {
		return
	}
	store, err := cache.Open(cache.Options{TTL: scanTTL})
	if err != nil {
		return
	}
	key := scanCacheKey(scanKeyFor(opts), opts.ipv6)
	raw, ok := store.Get(key)
	if !ok {
		return
	}
	var cs cachedScan
	if json.Unmarshal(raw, &cs) != nil {
		return
	}
	changed := false
	for i := range cs.Results {
		if cs.Results[i].Endpoint != ce.Endpoint {
			continue
		}
		if fn(&cs.Results[i]) {
			changed = true
		}
		break
	}
	if !changed {
		return
	}
	if raw, err := json.Marshal(cs); err == nil {
		_ = store.Put(key, raw)
	}
}

// markEndpointHandshakeFailed counts a failure against the base endpoint. It
// never flips Working to false: a relay can fail while the underlay is fine.
func markEndpointHandshakeFailed(opts options, ce cachedEndpoint) {
	mutateEndpointHealth(opts, ce, func(e *cachedEndpoint) bool {
		e.FailCount++
		if e.FailCount >= endpointFailLimit {
			e.QuarantinedUntil = time.Now().Add(endpointFailQuarantine)
		}
		return true
	})
}

// clearEndpointHandshakeFailure forgives an endpoint after a successful
// session, so a single bad day does not demote it forever.
func clearEndpointHandshakeFailure(opts options, ce cachedEndpoint) {
	mutateEndpointHealth(opts, ce, func(e *cachedEndpoint) bool {
		if e.FailCount == 0 && e.QuarantinedUntil.IsZero() {
			return false
		}
		e.FailCount = 0
		e.QuarantinedUntil = time.Time{}
		return true
	})
}
