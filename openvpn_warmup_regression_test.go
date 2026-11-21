package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestWarmupAndVerifySuccessContinuesToVerify pins the R002 regression: the
// warm-up result must have a producer, and a nil result must fall through to
// the data-plane verification instead of leaving the select blocked forever.
func TestWarmupAndVerifySuccessContinuesToVerify(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	opts := options{mixedPort: 7890, apiPort: 9090, openvpnMode: true}
	verified, stopped, watchStopped := false, false, false
	deps := openVPNWarmupDeps{
		warm: func(context.Context, int, string) error { return nil },
		watch: func(context.Context, int) (<-chan string, func()) {
			return make(chan string), func() { watchStopped = true }
		},
		verify: func(o options, logf func(string)) (clientState, error) {
			verified = true
			logf("数据面验证成功")
			return clientState{Connected: true, MeasuredBps: 200 * 1024}, nil
		},
		stopCore: func() error { stopped = true; return nil },
		stage:    func(string) {},
	}

	done := make(chan struct {
		state clientState
		err   error
	}, 1)
	go func() {
		st, err := warmupAndVerifyOpenVPN(ctx, opts, "AWG+OpenVPN", deps)
		done <- struct {
			state clientState
			err   error
		}{st, err}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("warmup+verify failed: %v", got.err)
		}
		if !got.state.Connected {
			t.Fatal("verified state was not returned")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("warmup select hung: warmDone has no producer again")
	}
	if !verified {
		t.Fatal("successful warm-up did not reach verifyTunnel")
	}
	if stopped || !watchStopped {
		t.Fatalf("cleanup mismatch: stopped=%v watchStopped=%v", stopped, watchStopped)
	}
	if openvpnWarmupActive.Load() {
		t.Fatal("warm-up suppression flag leaked")
	}
}

// TestWarmupAndVerifyCancelDoesNotHang proves the context branch exists and
// that cancellation stops the core and releases the watcher/flag.
func TestWarmupAndVerifyCancelDoesNotHang(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	opts := options{mixedPort: 7890, apiPort: 9090, openvpnMode: true}
	stopped, watchStopped := false, false
	deps := openVPNWarmupDeps{
		warm: func(warmCtx context.Context, _ int, _ string) error {
			<-warmCtx.Done()
			return warmCtx.Err()
		},
		watch: func(context.Context, int) (<-chan string, func()) {
			return make(chan string), func() { watchStopped = true }
		},
		verify: func(options, func(string)) (clientState, error) {
			t.Error("verifyTunnel must not run after cancellation")
			return clientState{}, nil
		},
		stopCore: func() error { stopped = true; return nil },
		stage:    func(string) {},
	}

	done := make(chan error, 1)
	go func() {
		_, err := warmupAndVerifyOpenVPN(ctx, opts, "AWG+OpenVPN", deps)
		done <- err
	}()
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("warmup select ignored context cancellation")
	}
	if !stopped || !watchStopped {
		t.Fatalf("cleanup mismatch: stopped=%v watchStopped=%v", stopped, watchStopped)
	}
	if openvpnWarmupActive.Load() {
		t.Fatal("warm-up suppression flag leaked after cancellation")
	}
}

// TestWarmupAndVerifyFatalLogWins covers the only branch that worked before
// the fix, so a future refactor cannot break it while adding the others.
func TestWarmupAndVerifyFatalLogWins(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	opts := options{mixedPort: 7890, apiPort: 9090, openvpnMode: true}
	fatal := make(chan string, 1)
	fatal <- "AUTH_FAILED"
	stopped := false
	deps := openVPNWarmupDeps{
		warm: func(warmCtx context.Context, _ int, _ string) error {
			<-warmCtx.Done()
			return warmCtx.Err()
		},
		watch: func(context.Context, int) (<-chan string, func()) {
			return fatal, func() {}
		},
		verify: func(options, func(string)) (clientState, error) {
			return clientState{}, fmt.Errorf("must not verify")
		},
		stopCore: func() error { stopped = true; return nil },
		stage:    func(string) {},
	}

	done := make(chan error, 1)
	go func() {
		_, err := warmupAndVerifyOpenVPN(ctx, opts, "AWG+OpenVPN", deps)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "AUTH_FAILED") {
			t.Fatalf("error = %v, want AUTH_FAILED", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fatal log branch hung")
	}
	if !stopped {
		t.Fatal("core was not stopped on fatal relay error")
	}
}
