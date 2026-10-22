package iflow

import (
	"net"
	"testing"
)

// TestLoopbackAddrBindsIPv4 ensures OAuth callback listeners actually bind a
// loopback address (not an unspecified/wildcard interface) and stay reachable
// through 127.0.0.1 regardless of how the host resolves "localhost".
func TestLoopbackAddrBindsIPv4(t *testing.T) {
	if got := loopbackAddr(8080); got != "127.0.0.1:8080" {
		t.Fatalf("loopbackAddr(8080) = %q, want 127.0.0.1:8080", got)
	}

	listener, err := net.Listen("tcp", loopbackAddr(0))
	if err != nil {
		t.Fatalf("listen on loopback address: %v", err)
	}
	defer func() { _ = listener.Close() }()

	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address = %T, want *net.TCPAddr", listener.Addr())
	}
	if !addr.IP.IsLoopback() {
		t.Fatalf("listener bound %s, want a loopback address", addr.IP)
	}
	if addr.IP.IsUnspecified() {
		t.Fatalf("listener bound the unspecified address %s", addr.IP)
	}
}
