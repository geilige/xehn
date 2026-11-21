package main

import (
	"testing"
	"time"
)

func TestRankHealthyEndpointsDemotesFailures(t *testing.T) {
	now := time.Now()
	cands := []cachedEndpoint{
		{Endpoint: "a", FailCount: 2, QuarantinedUntil: now.Add(time.Minute)},
		{Endpoint: "b"},
		{Endpoint: "c", FailCount: 1},
		{Endpoint: "d"},
	}
	out := rankHealthyEndpoints(cands)
	got := out[0].Endpoint + out[1].Endpoint + out[2].Endpoint + out[3].Endpoint
	if got != "bdca" {
		t.Fatalf("order = %s, want bdca", got)
	}
	// The caller's slice must not be reordered in place.
	if cands[0].Endpoint != "a" {
		t.Fatal("rankHealthyEndpoints mutated its input")
	}
}

func TestRankHealthyEndpointsKeepsCleanOrder(t *testing.T) {
	cands := []cachedEndpoint{{Endpoint: "b"}, {Endpoint: "a"}, {Endpoint: "c"}}
	out := rankHealthyEndpoints(cands)
	for i := range cands {
		if out[i].Endpoint != cands[i].Endpoint {
			t.Fatalf("clean endpoints were reordered: %v", out)
		}
	}
}

func TestOpenVPNAttemptsSettingNormalizes(t *testing.T) {
	s := defaultSettings()
	if s.OpenVPNAttempts != openvpnMaxAttempts {
		t.Fatalf("default attempts = %d, want %d", s.OpenVPNAttempts, openvpnMaxAttempts)
	}
	for _, tc := range []struct{ in, want int }{{0, openvpnMaxAttempts}, {3, 3}, {99, openvpnMaxAttempts}} {
		s.OpenVPNAttempts = tc.in
		s.normalize()
		if s.OpenVPNAttempts != tc.want {
			t.Fatalf("attempts %d normalized to %d, want %d", tc.in, s.OpenVPNAttempts, tc.want)
		}
	}
}
