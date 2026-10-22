package proto

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestDialH2StreamContextCancelsDuringDial ensures cancelling the caller's
// context aborts a dial that is still waiting for a connection.
func TestDialH2StreamContextCancelsDuringDial(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dialStarted := make(chan struct{})
	dial := func(dialCtx context.Context, network, addr string) (net.Conn, error) {
		close(dialStarted)
		<-dialCtx.Done()
		return nil, dialCtx.Err()
	}

	done := make(chan error, 1)
	go func() {
		_, err := DialH2StreamContextWithDialer(ctx, "api2.cursor.sh", map[string]string{}, dial)
		done <- err
	}()

	select {
	case <-dialStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("dial never started")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("DialH2StreamContextWithDialer error = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not abort the dial")
	}
}

// TestDialH2StreamContextCancelsHandshake ensures cancelling the context
// interrupts a TLS handshake that is still in progress.
func TestDialH2StreamContextCancelsHandshake(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer func() { _ = serverConn.Close() }()

	handshakeStarted := make(chan struct{})
	clientConn = &signalingConn{Conn: clientConn, started: handshakeStarted}
	dial := func(context.Context, string, string) (net.Conn, error) {
		return clientConn, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := DialH2StreamContextWithDialer(ctx, "api2.cursor.sh", map[string]string{}, dial)
		done <- err
	}()

	select {
	case <-handshakeStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("TLS handshake never started I/O")
	}
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("DialH2StreamContextWithDialer returned nil error after cancellation")
		}
		if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "handshake") {
			t.Fatalf("unexpected cancellation error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not abort the TLS handshake")
	}
}

// TestDialH2StreamContextWithDialerPropagatesDialErrors ensures a dial failure
// is reported with the dial context.
func TestDialH2StreamContextWithDialerPropagatesDialErrors(t *testing.T) {
	dial := func(dialCtx context.Context, network, addr string) (net.Conn, error) {
		if dialCtx == nil {
			t.Fatal("dialer received a nil context")
		}
		return nil, errors.New("boom")
	}

	_, err := DialH2StreamContextWithDialer(context.Background(), "api2.cursor.sh", map[string]string{}, dial)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("DialH2StreamContextWithDialer error = %v, want dial failure", err)
	}
}

// signalingConn reports the first handshake I/O attempt without sleeps.
type signalingConn struct {
	net.Conn
	started chan struct{}
	once    sync.Once
}

func (c *signalingConn) signal() { c.once.Do(func() { close(c.started) }) }

func (c *signalingConn) Read(p []byte) (int, error) {
	c.signal()
	return c.Conn.Read(p)
}

func (c *signalingConn) Write(p []byte) (int, error) {
	c.signal()
	return c.Conn.Write(p)
}
