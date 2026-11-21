//go:build !windows

package main

import "sync/atomic"

// openvpnWarmupActive is defined for real on Windows, where the GUI log
// pipeline suppresses expected warm-up timeouts. The CLI/other OS build keeps
// the same symbol so the connect pipeline stays platform-neutral.
var openvpnWarmupActive atomic.Bool
