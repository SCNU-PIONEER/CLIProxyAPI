package iflow

import "testing"

// TestLoopbackAddrKeepsCallbackServerLocal ensures OAuth callback servers never
// listen on a routable interface.
func TestLoopbackAddrKeepsCallbackServerLocal(t *testing.T) {
	if got := loopbackAddr(8317); got != "127.0.0.1:8317" {
		t.Fatalf("loopbackAddr(8317) = %q, want 127.0.0.1:8317", got)
	}
}
