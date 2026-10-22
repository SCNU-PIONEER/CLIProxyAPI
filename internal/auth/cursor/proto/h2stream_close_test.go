package proto

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

func newTestH2Stream() *H2Stream {
	s := &H2Stream{
		dataCh:  make(chan []byte),
		doneCh:  make(chan struct{}),
		closeCh: make(chan struct{}),
	}
	s.windowCond = sync.NewCond(&s.windowMu)
	return s
}

// TestH2StreamCloseUnblocksWindowStalledWrite ensures a writer waiting for
// flow-control credit does not sleep forever after the stream is closed.
func TestH2StreamCloseUnblocksWindowStalledWrite(t *testing.T) {
	s := newTestH2Stream()
	s.sendWindow, s.connWindow = 0, 0

	writeDone := make(chan error, 1)
	go func() { writeDone <- s.Write([]byte("payload")) }()

	select {
	case err := <-writeDone:
		t.Fatalf("Write returned before flow-control window was available: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	s.Close()

	select {
	case err := <-writeDone:
		if !errors.Is(err, ErrStreamClosed) {
			t.Fatalf("Write error after Close = %v, want ErrStreamClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Write stayed blocked in flow control after Close")
	}
}

// TestH2StreamWriteAfterCloseFails ensures writes never reach a closed conn.
func TestH2StreamWriteAfterCloseFails(t *testing.T) {
	s := newTestH2Stream()
	s.Close()

	done := make(chan error, 1)
	go func() { done <- s.Write([]byte("payload")) }()

	select {
	case err := <-done:
		if !errors.Is(err, ErrStreamClosed) {
			t.Fatalf("Write error = %v, want ErrStreamClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Write did not fail after Close")
	}
}

// TestH2StreamWriteResumesWhenWindowOpens covers the established path: a
// stalled writer continues once flow-control credit is granted.
func TestH2StreamWriteResumesWhenWindowOpens(t *testing.T) {
	var buf bytes.Buffer
	s := newTestH2Stream()
	s.framer = http2.NewFramer(&buf, nil)
	s.streamID = 1
	s.sendWindow, s.connWindow = 0, 0

	done := make(chan error, 1)
	go func() { done <- s.Write([]byte("hello")) }()

	// Window credit is published under windowMu before the broadcast, so a
	// writer that has not reached Wait yet observes the credit on its next
	// loop check; a writer already in Wait is woken by the broadcast. No
	// sleep is needed to avoid a lost wakeup, and this guards early success.
	select {
	case err := <-done:
		t.Fatalf("Write returned before flow-control credit was granted: %v", err)
	default:
	}
	s.windowMu.Lock()
	s.sendWindow, s.connWindow = 100, 100
	s.windowMu.Unlock()
	s.windowCond.Broadcast()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Write error = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Write did not resume after the window opened")
	}
	if buf.Len() == 0 {
		t.Fatal("Write reported success without writing a DATA frame")
	}
}

// TestH2StreamWriteSendsDataWithinWindow covers the established write path.
func TestH2StreamWriteSendsDataWithinWindow(t *testing.T) {
	var buf bytes.Buffer
	s := newTestH2Stream()
	s.framer = http2.NewFramer(&buf, nil)
	s.streamID = 1
	s.sendWindow, s.connWindow = 1024, 1024

	if err := s.Write([]byte("hello")); err != nil {
		t.Fatalf("Write error = %v, want nil", err)
	}
	if buf.Len() == 0 {
		t.Fatal("Write did not emit a DATA frame")
	}
	if got := s.sendWindow; got != 1019 {
		t.Fatalf("sendWindow = %d, want 1019", got)
	}
	if got := s.connWindow; got != 1019 {
		t.Fatalf("connWindow = %d, want 1019", got)
	}
}

// TestH2StreamDeliverUnblocksWhenReceiverGone ensures readLoop does not block
// forever handing a frame to a consumer that stopped reading.
func TestH2StreamDeliverUnblocksWhenReceiverGone(t *testing.T) {
	s := newTestH2Stream()

	delivered := make(chan bool, 1)
	go func() { delivered <- s.deliver([]byte("chunk")) }()

	select {
	case <-delivered:
		t.Fatal("deliver returned while the data channel had no receiver")
	case <-time.After(50 * time.Millisecond):
	}

	s.Close()

	select {
	case ok := <-delivered:
		if ok {
			t.Fatal("deliver reported success after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("deliver stayed blocked after Close")
	}
}

// TestH2StreamCloseIsIdempotent ensures concurrent or repeated Close calls are
// safe and still publish the closed state.
func TestH2StreamCloseIsIdempotent(t *testing.T) {
	s := newTestH2Stream()
	s.Close()
	s.Close()
	s.Close()

	select {
	case <-s.closeCh:
	default:
		t.Fatal("closeCh was not closed")
	}

	s.windowMu.Lock()
	closed := s.closed
	s.windowMu.Unlock()
	if !closed {
		t.Fatal("closed flag was not set")
	}
}

// TestH2StreamErrConcurrentWithSetErr exercises the synchronized error field.
func TestH2StreamErrConcurrentWithSetErr(t *testing.T) {
	s := newTestH2Stream()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			s.setErr(errors.New("burn"))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			_ = s.Err()
		}
	}()
	wg.Wait()
}
