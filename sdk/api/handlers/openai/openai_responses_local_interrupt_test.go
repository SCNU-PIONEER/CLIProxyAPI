package openai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/tidwall/gjson"
)

func beginLocalInterruptTurn(local *responsesLocalInterrupt, ctx context.Context, responseID string) <-chan []byte {
	local.begin(ctx)
	return local.rearm(responseID)
}

// A delivery captured before the turn ended must not be reported as accepted
// once teardown closes the handoff: the reader needs that false result to send
// the sanitized error instead of silently dropping the interrupt.
func TestResponsesLocalInterruptDoesNotReportAbandonedDelivery(t *testing.T) {
	local := newResponsesLocalInterrupt()
	t.Cleanup(local.end)
	beginLocalInterruptTurn(local, context.Background(), "r-http")

	started := make(chan struct{})
	result := make(chan bool, 1)
	go func() {
		close(started)
		result <- local.deliver([]byte(`{"type":"response.interrupt","response_id":"r-http"}`))
	}()
	<-started
	// The forward loop is already gone; end is its deferred teardown.
	local.end()
	select {
	case delivered := <-result:
		if delivered {
			t.Fatal("abandoned delivery was reported as consumed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("deliver did not return after teardown")
	}
}

func TestResponsesLocalInterruptDeliversToLiveConsumer(t *testing.T) {
	local := newResponsesLocalInterrupt()
	t.Cleanup(local.end)
	frames := beginLocalInterruptTurn(local, context.Background(), "r-http")
	result := make(chan bool, 1)
	go func() {
		result <- local.deliver([]byte(`{"type":"response.interrupt","response_id":"r-http"}`))
	}()
	select {
	case payload := <-frames:
		if len(payload) == 0 {
			t.Fatal("empty interrupt payload")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("live consumer did not receive the interrupt")
	}
	select {
	case delivered := <-result:
		if !delivered {
			t.Fatal("delivery to a live consumer was rejected")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("deliver did not return after handoff")
	}
}

// Only the exact response created for the current turn may be canceled
// locally; unknown, stale, or non-string IDs must be rejected.
func TestResponsesLocalInterruptRequiresCurrentResponseID(t *testing.T) {
	local := newResponsesLocalInterrupt()
	t.Cleanup(local.end)
	beginLocalInterruptTurn(local, context.Background(), "r-http")
	for _, payload := range []string{
		`{"type":"response.interrupt","response_id":"unknown"}`,
		`{"type":"response.interrupt","response_id":"123"}`,
		`{"type":"response.interrupt","response_id":123}`,
		`{"type":"response.interrupt"}`,
	} {
		if local.deliver([]byte(payload)) {
			t.Fatalf("delivery was accepted for %s", payload)
		}
	}
}

func TestResponsesLocalInterruptDisarmRejectsDelivery(t *testing.T) {
	local := newResponsesLocalInterrupt()
	t.Cleanup(local.end)
	beginLocalInterruptTurn(local, context.Background(), "r-http")
	local.disarm()
	if local.deliver([]byte(`{"type":"response.interrupt","response_id":"r-http"}`)) {
		t.Fatal("delivery after disarm was accepted")
	}
	// Teardown after a terminal response must be idempotent.
	local.disarm()
	local.end()
}

// A stale handoff must be rejected even when the next turn already receives on
// its own channel: rearm replaces both frames and done.
func TestResponsesLocalInterruptRearmReplacesPerTurnChannels(t *testing.T) {
	local := newResponsesLocalInterrupt()
	t.Cleanup(local.end)
	local.begin(context.Background())
	local.mu.Lock()
	oldFrames := local.frames
	oldDone := local.done
	local.mu.Unlock()

	newFrames := local.rearm("turn-2")
	if newFrames == oldFrames {
		t.Fatal("rearm reused the previous turn's frames channel")
	}
	select {
	case <-oldDone:
	default:
		t.Fatal("rearm did not close the previous turn's done channel")
	}

	// Emulate a delivery that captured the old channels before rearm: it can
	// only observe done, never hand the stale frame to the new turn.
	stale := make(chan bool, 1)
	go func() {
		select {
		case oldFrames <- []byte(`{"type":"response.interrupt","response_id":"turn-1"}`):
			stale <- true
		case <-oldDone:
			stale <- false
		}
	}()
	select {
	case delivered := <-stale:
		if delivered {
			t.Fatal("stale delivery reached the new turn's receiver")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stale delivery did not observe the previous done channel")
	}

	result := make(chan bool, 1)
	go func() {
		result <- local.deliver([]byte(`{"type":"response.interrupt","response_id":"turn-2"}`))
	}()
	select {
	case payload := <-newFrames:
		if len(payload) == 0 {
			t.Fatal("empty rearmed interrupt payload")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("new turn did not receive on the rearmed channel")
	}
	select {
	case delivered := <-result:
		if !delivered {
			t.Fatal("delivery after rearm was rejected")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("rearmed deliver did not return")
	}
}

// begin is also used to restart a fresh forward loop; it must not leave an
// older blocked deliver orphaned on the replaced channels.
func TestResponsesLocalInterruptBeginClosesPreviousDone(t *testing.T) {
	local := newResponsesLocalInterrupt()
	t.Cleanup(local.end)
	local.begin(context.Background())
	local.mu.Lock()
	oldDone := local.done
	local.mu.Unlock()

	newFrames := local.begin(context.Background())
	select {
	case <-oldDone:
	default:
		t.Fatal("begin did not close the previous done channel")
	}
	if newFrames == nil {
		t.Fatal("begin returned no frames channel")
	}
}

// The reader must not depend on a downstream writer draining the handoff: a
// canceled forward context releases a blocked deliver.
func TestResponsesLocalInterruptCanceledContextReleasesDeliver(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	local := newResponsesLocalInterrupt()
	t.Cleanup(local.end)
	beginLocalInterruptTurn(local, ctx, "r-http")

	started := make(chan struct{})
	result := make(chan bool, 1)
	go func() {
		close(started)
		result <- local.deliver([]byte(`{"type":"response.interrupt","response_id":"r-http"}`))
	}()
	<-started
	cancel()
	select {
	case delivered := <-result:
		if delivered {
			t.Fatal("delivery was accepted after context cancellation")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled context did not release the blocked deliver")
	}
}

// Terminal IDs are bounded FIFO state: recent IDs stay known, the oldest is
// evicted, and non-string IDs are never coerced into a match.
func TestResponsesLocalInterruptTerminalLedgerIsBoundedAndTyped(t *testing.T) {
	local := newResponsesLocalInterrupt()
	t.Cleanup(local.end)
	local.begin(context.Background())

	local.rememberTerminal("h-oldest")
	for i := 0; i < localInterruptTerminalHistory-1; i++ {
		local.rememberTerminal(fmt.Sprintf("h-%d", i))
	}
	if !local.knownTerminal([]byte(`{"response_id":"h-oldest"}`)) {
		t.Fatal("retained terminal ID was not known")
	}
	local.rememberTerminal("h-newest")
	if local.knownTerminal([]byte(`{"response_id":"h-oldest"}`)) {
		t.Fatal("oldest terminal ID was not evicted at the bound")
	}
	if !local.knownTerminal([]byte(`{"response_id":"h-newest"}`)) {
		t.Fatal("newest terminal ID was not known")
	}
	if local.knownTerminal([]byte(`{"response_id":123}`)) {
		t.Fatal("non-string response_id matched a terminal")
	}
	if local.knownTerminal([]byte(`{"response_id":""}`)) {
		t.Fatal("empty response_id matched a terminal")
	}
}

// A terminal response closes local cancellation; a later interrupt for it is
// handled as a no-op rather than delivered to the next active turn.
func TestResponsesLocalInterruptKnownTerminalAfterDisarm(t *testing.T) {
	local := newResponsesLocalInterrupt()
	t.Cleanup(local.end)
	local.begin(context.Background())
	local.rearm("h1")
	local.rememberTerminal("h1")
	local.disarm()
	// Later turns reuse the same connection and only rearm; the ledger stays.
	frames := local.rearm("h2")
	if !local.knownTerminal([]byte(`{"type":"response.interrupt","response_id":"h1"}`)) {
		t.Fatal("terminal h1 was not remembered across turns")
	}
	if local.deliver([]byte(`{"type":"response.interrupt","response_id":"h1"}`)) {
		t.Fatal("terminal h1 interrupt canceled the next turn")
	}
	delivered := make(chan bool, 1)
	go func() {
		delivered <- local.deliver([]byte(`{"type":"response.interrupt","response_id":"h2"}`))
	}()
	select {
	case <-frames:
	case <-time.After(3 * time.Second):
		t.Fatal("active h2 interrupt was not received")
	}
	select {
	case ok := <-delivered:
		if !ok {
			t.Fatal("active h2 interrupt was not delivered")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("active h2 deliver did not return")
	}
}

func TestResponsesLocalInterruptFailedTerminalDisarms(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	local := &responsesLocalInterrupt{}
	t.Cleanup(local.end)
	frames := beginLocalInterruptTurn(local, ctx, "h1")
	if frames == nil {
		t.Fatal("expected an armed turn")
	}
	payload := []byte(`{"type":"response.failed","response":{"id":"h1","status":"failed"}}`)
	if !local.observeTerminal("response.failed", payload) {
		t.Fatal("response.failed should be terminal")
	}
	if !local.knownTerminal([]byte(`{"response_id":"h1"}`)) {
		t.Fatal("failed terminal was not recorded")
	}
	if local.deliver([]byte(`{"response_id":"h1"}`)) {
		t.Fatal("delivery after a failed terminal must be rejected")
	}
}

// TestForwardLocalInterruptRecordsLocallyGeneratedTerminal drives the real
// forward loop: an interrupt delivered on the local handoff makes forward emit
// response.incomplete, and the generated terminal ID must already be a ledger
// entry by then. Without that, a duplicate interrupt racing the local terminal
// could only be rejected instead of treated as a handled no-op.
func TestForwardLocalInterruptRecordsLocallyGeneratedTerminal(t *testing.T) {
	gin.SetMode(gin.TestMode)

	data := make(chan []byte)
	errs := make(chan *interfaces.ErrorMessage)
	result := make(chan string, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, errUpgrade := responsesWebsocketUpgrader.Upgrade(w, r, nil)
		if errUpgrade != nil {
			result <- "upgrade: " + errUpgrade.Error()
			return
		}
		defer func() { _ = conn.Close() }()
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = r
		h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
		local := newResponsesLocalInterrupt()
		forwardErr := make(chan error, 1)
		go func() {
			_, _, _, _, errForward := h.forwardResponsesWebsocket(
				ctx,
				newResponsesWebsocketWriter(conn),
				func(...interface{}) {},
				data,
				errs,
				newInMemoryWebsocketTimelineLog(),
				"session-local-terminal",
				responsesWebsocketForwardOptions{localInterrupt: local},
			)
			forwardErr <- errForward
		}()
		data <- []byte(`{"type":"response.created","response":{"id":"h2"}}`)
		interrupt := []byte(`{"type":"response.interrupt","response_id":"h2"}`)
		go func() {
			for !local.deliver(interrupt) {
				runtime.Gosched()
			}
		}()
		errForward := <-forwardErr
		if errForward != nil {
			result <- "forward: " + errForward.Error()
			return
		}
		known := local.knownTerminal([]byte(`{"response_id":"h2"}`))
		redelivered := local.deliver(interrupt)
		result <- fmt.Sprintf("known=%t redelivered=%t", known, redelivered)
	}))
	defer server.Close()

	client, _, errDial := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if errDial != nil {
		t.Fatal(errDial)
	}
	defer func() { _ = client.Close() }()
	if errDeadline := client.SetReadDeadline(time.Now().Add(5 * time.Second)); errDeadline != nil {
		t.Fatal(errDeadline)
	}
	for {
		_, payload, errRead := client.ReadMessage()
		if errRead != nil {
			t.Fatal(errRead)
		}
		if gjson.GetBytes(payload, "type").String() != "response.incomplete" {
			continue
		}
		if gjson.GetBytes(payload, "response.id").String() != "h2" {
			t.Fatalf("expected a local response.incomplete for h2, got %s", payload)
		}
		break
	}
	if got := <-result; got != "known=true redelivered=false" {
		t.Fatalf("local terminal not recorded before publish: %s", got)
	}
}
