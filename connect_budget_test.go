package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestWarmupWindowHasMargin pins the R003 parameter regression: a 17s window
// matched the observed handshake exactly, leaving no room for network jitter.
func TestWarmupWindowHasMargin(t *testing.T) {
	if openvpnHandshakeTimeout < 20 {
		t.Fatalf("handshake timeout = %d, want at least 20s", openvpnHandshakeTimeout)
	}
	if openvpnWarmupWindow < 20*time.Second {
		t.Fatalf("warmup window = %s, want at least 20s", openvpnWarmupWindow)
	}
	if openvpnWarmupWindow <= observedOpenVPNHandshakeBaseline {
		t.Fatalf("warmup window %s must exceed observed handshake %s", openvpnWarmupWindow, observedOpenVPNHandshakeBaseline)
	}
	margin := openvpnWarmupWindow - observedOpenVPNHandshakeBaseline
	if margin < 3*time.Second {
		t.Fatalf("warmup margin = %s, want at least 3s", margin)
	}
}

// TestEffectiveConnectLimitCoversConfiguredAttempts ensures "6 attempts" is
// not silently reduced to four by an unchanged 90s ceiling.
func TestEffectiveConnectLimitCoversConfiguredAttempts(t *testing.T) {
	if got := effectiveConnectLimit(false, 6); got != connectHardLimit {
		t.Fatalf("plain connect limit = %s, want %s", got, connectHardLimit)
	}
	if got := effectiveConnectLimit(true, 2); got != connectHardLimit {
		t.Fatalf("2-attempt OpenVPN limit = %s, want the 90s floor", got)
	}
	for _, attempts := range []int{6, 8, 12} {
		got := effectiveConnectLimit(true, attempts)
		want := time.Duration(attempts)*openvpnAttemptBudget + connectLimitSlack
		if got < want {
			t.Fatalf("%d attempts: limit %s < needed %s", attempts, got, want)
		}
		if got <= connectHardLimit {
			t.Fatalf("%d attempts: limit %s did not grow past 90s", attempts, got)
		}
	}
}

// TestNextRoundContextResetsBudget pins R006 problem two: an internal rescan
// must get a fresh full six-combination round, not the previous round's
// remaining seconds.
func TestNextRoundContextResetsBudget(t *testing.T) {
	root, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &connectBudget{absolute: time.Now().Add(connectAbsoluteLimit)}
	ctx := withConnectBudget(root, b)
	b.root = ctx
	opts := options{openvpnMode: true, openvpnAttempts: 6}

	first, cancelFirst, err := nextRoundContext(ctx, opts)
	if err != nil {
		t.Fatalf("first round: %v", err)
	}
	cancelFirst() // simulate the first round finishing
	second, cancelSecond, err := nextRoundContext(ctx, opts)
	if err != nil {
		t.Fatalf("second round: %v", err)
	}
	defer cancelSecond()

	want := effectiveConnectLimit(true, 6)
	if left := time.Until(deadlineOf(t, second)); left < want-time.Second {
		t.Fatalf("second round has %s, want a fresh %s budget", left, want)
	}
	if reconnectContext(first) != ctx {
		t.Fatal("reconnectContext must return the budget-bearing root context")
	}
}

func TestNextRoundContextAbsoluteCap(t *testing.T) {
	root, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &connectBudget{root: root, absolute: time.Now().Add(time.Second)}
	ctx := withConnectBudget(root, b)
	_, _, err := nextRoundContext(ctx, options{openvpnMode: true, openvpnAttempts: 6})
	if !errors.Is(err, errConnectTotalTimeout) {
		t.Fatalf("error = %v, want errConnectTotalTimeout", err)
	}
}

func deadlineOf(t *testing.T, ctx context.Context) time.Time {
	t.Helper()
	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("round context has no deadline")
	}
	return dl
}

func TestBudgetForNextAttempt(t *testing.T) {
	if !budgetForNextAttempt(context.Background()) {
		t.Fatal("a context without a deadline must allow another attempt")
	}
	short, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if budgetForNextAttempt(short) {
		t.Fatal("a context with less than one attempt budget must stop")
	}
	long, cancelLong := context.WithTimeout(context.Background(), openvpnAttemptBudget+time.Minute)
	defer cancelLong()
	if !budgetForNextAttempt(long) {
		t.Fatal("a context with a full attempt budget must continue")
	}
}
