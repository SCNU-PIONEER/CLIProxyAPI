package home

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// rebalanceTestDialer hands out pipe connections and remembers the peer side so
// tests can observe whether the transport end was closed.
type rebalanceTestDialer struct {
	mu      sync.Mutex
	client  []net.Conn
	peer    []net.Conn
	started chan struct{}
	release <-chan struct{}
}

func (d *rebalanceTestDialer) Dial(context.Context, string, string) (net.Conn, error) {
	clientConn, peerConn := net.Pipe()
	d.mu.Lock()
	d.client = append(d.client, clientConn)
	d.peer = append(d.peer, peerConn)
	d.mu.Unlock()
	if d.started != nil {
		close(d.started)
	}
	if d.release != nil {
		<-d.release
	}
	return clientConn, nil
}

func (d *rebalanceTestDialer) peers() []net.Conn {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]net.Conn(nil), d.peer...)
}

func assertConnClosed(t *testing.T, conn net.Conn) {
	t.Helper()
	if conn == nil {
		t.Fatal("nil connection")
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var buf [1]byte
	if _, errRead := conn.Read(buf[:]); errRead == nil {
		t.Fatal("connection still readable after transport cancellation")
	} else if errors.Is(errRead, io.EOF) || errors.Is(errRead, net.ErrClosed) || errors.Is(errRead, io.ErrClosedPipe) {
		return
	} else if netErr, ok := errRead.(net.Error); ok && netErr.Timeout() {
		t.Fatalf("connection left open after transport cancellation: %v", errRead)
	}
}

func TestRebalanceTransportTrackerClosesDialedConnections(t *testing.T) {
	dialer := &rebalanceTestDialer{}
	tracker := newRebalanceTransportTracker(dialer.Dial)
	if _, errDial := tracker.Dial(context.Background(), "tcp", "127.0.0.1:0"); errDial != nil {
		t.Fatalf("first dial: %v", errDial)
	}
	if _, errDial := tracker.Dial(context.Background(), "tcp", "127.0.0.1:0"); errDial != nil {
		t.Fatalf("second dial: %v", errDial)
	}
	if len(dialer.peers()) != 2 {
		t.Fatalf("dialed connections = %d, want 2", len(dialer.peers()))
	}

	tracker.close()

	for _, peer := range dialer.peers() {
		assertConnClosed(t, peer)
	}
}

func TestRebalanceTransportTrackerClosesConnectionDialedAfterCancel(t *testing.T) {
	dialer := &rebalanceTestDialer{}
	tracker := newRebalanceTransportTracker(dialer.Dial)
	tracker.close()

	if _, errDial := tracker.Dial(context.Background(), "tcp", "127.0.0.1:0"); !errors.Is(errDial, net.ErrClosed) {
		t.Fatalf("late dial error = %v, want net.ErrClosed", errDial)
	}
	peers := dialer.peers()
	if len(peers) != 1 {
		t.Fatalf("dialed connections = %d, want 1", len(peers))
	}
	assertConnClosed(t, peers[0])
}

func TestRebalanceTransportTrackerClosesConnectionCompletedAfterCancel(t *testing.T) {
	release := make(chan struct{})
	dialer := &rebalanceTestDialer{started: make(chan struct{}), release: release}
	tracker := newRebalanceTransportTracker(dialer.Dial)
	dialDone := make(chan error, 1)
	go func() {
		_, errDial := tracker.Dial(context.Background(), "tcp", "127.0.0.1:0")
		dialDone <- errDial
	}()
	<-dialer.started
	tracker.close()
	close(release)

	if errDial := <-dialDone; !errors.Is(errDial, net.ErrClosed) {
		t.Fatalf("canceled dial error = %v, want net.ErrClosed", errDial)
	}
	peers := dialer.peers()
	if len(peers) != 1 {
		t.Fatalf("dialed connections = %d, want 1", len(peers))
	}
	assertConnClosed(t, peers[0])
}

func TestRebalanceTransportTrackerCancellationRace(t *testing.T) {
	for i := 0; i < 200; i++ {
		dialer := &rebalanceTestDialer{}
		tracker := newRebalanceTransportTracker(dialer.Dial)
		dialDone := make(chan error, 1)
		go func() {
			_, errDial := tracker.Dial(context.Background(), "tcp", "127.0.0.1:0")
			dialDone <- errDial
		}()
		tracker.close()
		errDial := <-dialDone
		if errDial != nil && !errors.Is(errDial, net.ErrClosed) {
			t.Fatalf("iteration %d: dial error = %v", i, errDial)
		}
		for _, peer := range dialer.peers() {
			assertConnClosed(t, peer)
		}
	}
}

func TestRebalanceCommandCancelAbortsBlockedTransport(t *testing.T) {
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("listen: %v", errListen)
	}
	commandSeen := make(chan struct{})
	var once sync.Once
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		for {
			conn, errAccept := listener.Accept()
			if errAccept != nil {
				return
			}
			go func() {
				defer conn.Close()
				var buf [512]byte
				if _, errRead := conn.Read(buf[:]); errRead != nil {
					return
				}
				once.Do(func() { close(commandSeen) })
				// Hold the connection open without replying; cancellation must
				// close the transport instead of waiting for a read timeout.
				_, _ = io.Copy(io.Discard, conn)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-serverDone
	})

	host, portText, errSplit := net.SplitHostPort(listener.Addr().String())
	if errSplit != nil {
		t.Fatalf("split addr: %v", errSplit)
	}
	port := mustAtoi(t, portText)
	client := New(config.HomeConfig{Enabled: true, Host: host, Port: port, DisableClusterDiscovery: true})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, errCommand := client.rebalanceCommand(ctx, "", "CLUSTER", "BALANCE", "ENABLE")
		done <- errCommand
	}()

	select {
	case <-commandSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("rebalance command did not reach test server")
	}

	cancel()
	select {
	case errCommand := <-done:
		if errCommand == nil {
			t.Fatal("canceled rebalance command returned nil error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled rebalance command stayed blocked on transport close")
	}
}

func mustAtoi(t *testing.T, text string) int {
	t.Helper()
	value, errAtoi := strconv.Atoi(text)
	if errAtoi != nil {
		t.Fatalf("atoi %q: %v", text, errAtoi)
	}
	return value
}
