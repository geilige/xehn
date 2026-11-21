package main

import "testing"

// The failure counter is the difference between a session that shakes off a
// hiccup and one that throws away a working endpoint: a torn-down endpoint
// shows up here as the same generic error a single DNS timeout does, so the
// counting is what tells them apart. These lock that down.
func TestJudgeHealthTransientFailureDoesNotRotate(t *testing.T) {
	cases := []struct {
		name       string
		fails      int
		ok         bool
		wantFails  int
		wantRepair bool
		wantRecov  bool
	}{
		{"healthy stays healthy", 0, true, 0, false, false},
		{"one failure counted, no rotation", 0, false, 1, false, false},
		{"two failures counted, no rotation", 1, false, 2, false, false},
		{"third failure rotates", 2, false, 0, true, false},
		{"recovered after one failure clears it", 1, true, 0, false, true},
		{"recovered after two failures clears it", 2, true, 0, false, true},
		{"counting restarts after a rotation", 0, false, 1, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := judgeHealth(tc.fails, tc.ok)
			if got.fails != tc.wantFails || got.repair != tc.wantRepair || got.recovered != tc.wantRecov {
				t.Fatalf("judgeHealth(%d,%v) = %+v, want fails=%d repair=%v recovered=%v",
					tc.fails, tc.ok, got, tc.wantFails, tc.wantRepair, tc.wantRecov)
			}
		})
	}
}

// A whole bad streak then one good answer: the endpoint must survive it. This
// is the case a heavier hand gets wrong - the failures are real, they just
// stopped being true.
func TestJudgeHealthStreakThenRecovery(t *testing.T) {
	fails := 0
	for i := 0; i < healthFailLimit-1; i++ {
		v := judgeHealth(fails, false)
		if v.repair {
			t.Fatalf("rotated after %d failures", i+1)
		}
		fails = v.fails
	}
	v := judgeHealth(fails, true)
	if v.repair || v.fails != 0 || !v.recovered {
		t.Fatalf("judgeHealth(%d,true) = %+v, want cleared and reported recovered", fails, v)
	}
}

// R008: the guard's third failure must be reconfirmed with a longer budget
// before it is allowed to rotate. The pure decision is tested here so the
// "one slow answer is not a dead exit" rule cannot regress silently.
func TestJudgeHealthWithReconfirm(t *testing.T) {
	first := judgeHealthWithReconfirm(0, false, false)
	if first.fails != 1 || first.repair {
		t.Fatalf("first failure = %+v, want fails=1 no repair", first)
	}
	second := judgeHealthWithReconfirm(first.fails, false, false)
	if second.fails != 2 || second.repair {
		t.Fatalf("second failure = %+v, want fails=2 no repair", second)
	}

	passed := judgeHealthWithReconfirm(second.fails, false, true)
	if passed.repair || passed.fails != 0 || !passed.recovered {
		t.Fatalf("third failure with passing reconfirm = %+v, want cleared and recovered", passed)
	}
	// The streak starts over: two more failures still must not rotate.
	again := judgeHealthWithReconfirm(passed.fails, false, false)
	if again.fails != 1 || again.repair {
		t.Fatalf("failure after reconfirm = %+v, want a fresh count of 1", again)
	}

	failed := judgeHealthWithReconfirm(second.fails, false, false)
	if !failed.repair || failed.fails != 0 {
		t.Fatalf("third failure with failed reconfirm = %+v, want repair", failed)
	}
}

func TestHealthReconfirmBudgetIsLonger(t *testing.T) {
	if healthReconfirmBudget < 2*probeBudget {
		t.Fatalf("reconfirm budget %s must be at least 2x normal %s", healthReconfirmBudget, probeBudget)
	}
	if healthReconfirmTimeout <= probeTimeout {
		t.Fatalf("reconfirm timeout %s must exceed normal %s", healthReconfirmTimeout, probeTimeout)
	}
}

// processAlive must not claim liveness for pids that cannot be ours: ownership
// is decided by this answer, and a wrong "alive" keeps a session unmonitored.
func TestProcessAliveRejectsImpossiblePids(t *testing.T) {
	for _, pid := range []int{0, -1} {
		if processAlive(pid) {
			t.Fatalf("processAlive(%d) = true, want false", pid)
		}
	}
}
