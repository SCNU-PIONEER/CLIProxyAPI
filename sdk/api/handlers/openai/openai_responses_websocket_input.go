package openai

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

// responsesLocalInterrupt cancels an in-flight HTTP turn. A websocket upstream
// does not use it; that interrupt is written to the existing socket instead.
//
// Delivery is a handoff to the active forward loop, not a queue: a frame is
// accepted only while that loop is receiving on the per-turn channel. The loop
// disarms the handoff before publishing a terminal response event, and
// the deferred end closes done, so an interrupt that arrives after the turn can
// never be reported as delivered and silently dropped.
const (
	// localInterruptTerminalHistory bounds terminal response IDs remembered per
	// forward connection so a late interrupt for an old HTTP turn stays a no-op.
	localInterruptTerminalHistory = 32
)

type responsesLocalInterrupt struct {
	mu     sync.Mutex
	active bool
	frames chan []byte
	done   chan struct{}
	// ctx is the forward loop's context. Cancellation releases a pending
	// handoff even when the forward loop is blocked writing downstream.
	ctx context.Context
	// activeResponseID is the response created for the current turn. Local
	// delivery requires an exact match so an unknown or stale ID cannot cancel
	// a live HTTP turn.
	activeResponseID string
	// terminalIDs/terminalOrder remember recent terminal response IDs (FIFO)
	// so a late interrupt for an HTTP turn that already ended is a no-op.
	terminalIDs   map[string]struct{}
	terminalOrder []string
}

func newResponsesLocalInterrupt() *responsesLocalInterrupt {
	return &responsesLocalInterrupt{}
}

// resetLocked closes the previous turn's handoff (releasing a blocked deliver)
// and installs fresh per-turn channels. Every turn owns its own frames and done
// so a stale delivery can never be handed to the next turn.
func (s *responsesLocalInterrupt) resetLocked(ctx context.Context, responseID string) <-chan []byte {
	if s.done != nil {
		close(s.done)
	}
	s.active = true
	s.frames = make(chan []byte)
	s.done = make(chan struct{})
	s.ctx = ctx
	s.activeResponseID = responseID
	return s.frames
}

func (s *responsesLocalInterrupt) begin(ctx context.Context) <-chan []byte {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resetLocked(ctx, "")
}

// disarm closes the current turn's handoff if it is still open. It is safe to
// call more than once and releases a deliver blocked on the handoff.
func (s *responsesLocalInterrupt) disarm() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.active = false
	s.activeResponseID = ""
	if s.done != nil {
		close(s.done)
		s.done = nil
	}
	s.mu.Unlock()
}

// rearm installs a fresh handoff for a later response.created on a long-lived
// duplex forward loop. The caller must receive on the returned channel.
func (s *responsesLocalInterrupt) rearm(responseID string) <-chan []byte {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resetLocked(s.ctx, responseID)
}

// rememberTerminal records a terminal response ID before its frame is written.
// The ledger is bounded by localInterruptTerminalHistory; the oldest entry is
// evicted first.
func (s *responsesLocalInterrupt) rememberTerminal(responseID string) {
	if s == nil || responseID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.terminalIDs == nil {
		s.terminalIDs = make(map[string]struct{})
	}
	if _, exists := s.terminalIDs[responseID]; exists {
		return
	}
	s.terminalIDs[responseID] = struct{}{}
	s.terminalOrder = append(s.terminalOrder, responseID)
	if len(s.terminalOrder) > localInterruptTerminalHistory {
		oldest := s.terminalOrder[0]
		s.terminalOrder = s.terminalOrder[1:]
		delete(s.terminalIDs, oldest)
	}
}

// observeTerminal reports whether eventType ends a turn for local delivery. On
// a terminal event it records the response ID before the frame is published and
// closes local cancellation, so a later interrupt for the turn is a no-op.
func (s *responsesLocalInterrupt) observeTerminal(eventType string, payload []byte) bool {
	switch eventType {
	case wsEventTypeCompleted, wsEventTypeDone, "response.incomplete", "response.failed":
	default:
		return false
	}
	if s == nil {
		return true
	}
	if responseID := gjson.GetBytes(payload, "response.id"); responseID.Type == gjson.String {
		s.rememberTerminal(strings.TrimSpace(responseID.String()))
	}
	s.disarm()
	return true
}

// knownTerminal reports whether the interrupt targets a terminal response of
// this forward connection.
func (s *responsesLocalInterrupt) knownTerminal(payload []byte) bool {
	responseID, ok := localInterruptResponseID(payload)
	if !ok {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, exists := s.terminalIDs[responseID]
	return exists
}

// localInterruptResponseID validates an interrupt's response_id. A non-string
// value must never be coerced into a matching terminal ID.
func localInterruptResponseID(payload []byte) (string, bool) {
	responseID := gjson.GetBytes(payload, "response_id")
	if responseID.Type != gjson.String {
		return "", false
	}
	id := strings.TrimSpace(responseID.String())
	return id, id != ""
}

func (s *responsesLocalInterrupt) end() {
	s.disarm()
}

// deliver hands payload to the active forward loop. It never blocks while
// holding the mutex, and teardown closes done so a blocked handoff returns.
func (s *responsesLocalInterrupt) deliver(payload []byte) bool {
	if s == nil {
		return false
	}
	responseID, ok := localInterruptResponseID(payload)
	if !ok {
		return false
	}
	s.mu.Lock()
	if !s.active || s.frames == nil || s.done == nil || responseID != s.activeResponseID {
		s.mu.Unlock()
		return false
	}
	frames, done, ctx := s.frames, s.done, s.ctx
	s.mu.Unlock()
	var ctxDone <-chan struct{}
	if ctx != nil {
		ctxDone = ctx.Done()
	}
	select {
	case frames <- bytes.Clone(payload):
		return true
	case <-done:
		return false
	case <-ctxDone:
		return false
	}
}

// readResponsesWebsocketInput is the only downstream reader. Interrupts are
// handled immediately so they are not queued behind an active response.
// The bounded queue backpressures clients instead of retaining unlimited input.
func readResponsesWebsocketInput(ctx context.Context, cancel context.CancelCauseFunc, conn *websocket.Conn, interrupt func([]byte) error, local *responsesLocalInterrupt, writer *responsesWebsocketWriter, timeline websocketTimelineAppender) (<-chan cliproxyexecutor.WebsocketInput, <-chan struct{}) {
	input := make(chan cliproxyexecutor.WebsocketInput, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(input)
		for {
			kind, payload, errRead := conn.ReadMessage()
			if errRead != nil {
				cancel(errRead)
				return
			}
			if kind != websocket.TextMessage && kind != websocket.BinaryMessage {
				continue
			}
			// Control frames bypass response.create normalization. The original
			// response_id, mode, and extension fields must reach upstream unchanged.
			if json.Valid(payload) && gjson.GetBytes(payload, "type").String() == "response.interrupt" {
				appendResponsesInterruptDiagnostic(timeline, payload, "")
				errInterrupt := interrupt(payload)
				switch {
				case errInterrupt == nil:
					appendResponsesInterruptDiagnostic(timeline, payload, "handled")
					continue
				case errors.Is(errInterrupt, cliproxyexecutor.ErrNoActiveUpstreamWebsocket):
					if local.knownTerminal(payload) {
						appendResponsesInterruptDiagnostic(timeline, payload, "handled")
						continue
					}
					if local.deliver(payload) {
						appendResponsesInterruptDiagnostic(timeline, payload, "local_http")
						continue
					}
					fallthrough
				default:
					appendResponsesInterruptDiagnostic(timeline, payload, "rejected")
					// The sanitized outcome replaces raw error logging here: errors
					// from transport dependencies may contain credentials or URLs.
					_, errWrite := writeResponsesWebsocketError(writer, nil, &interfaces.ErrorMessage{
						StatusCode: http.StatusBadRequest,
						Error:      errInterrupt,
					})
					if errWrite != nil {
						cancel(errWrite)
						return
					}
					continue
				}
			}
			select {
			case input <- cliproxyexecutor.WebsocketInput{Payload: payload}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return input, done
}

func appendResponsesInterruptDiagnostic(timeline websocketTimelineAppender, payload []byte, outcome string) {
	if timeline == nil {
		return
	}
	// Hash client-controlled IDs and omit all arbitrary fields, including mode.
	// Only correlation metadata is needed to diagnose control-frame timing.
	idHash := sha256.Sum256([]byte(gjson.GetBytes(payload, "response_id").String()))
	diagnostic := struct {
		Type           string `json:"type"`
		ResponseIDHash string `json:"response_id_hash"`
		Outcome        string `json:"outcome,omitempty"`
	}{Type: "response.interrupt", ResponseIDHash: fmt.Sprintf("%x", idHash[:8]), Outcome: outcome}
	data, errMarshal := json.Marshal(diagnostic)
	if errMarshal != nil {
		return
	}
	event := "interrupt"
	if outcome != "" {
		event = "interrupt.outcome"
	}
	timeline.Append(event, data, time.Now())
}

func (h *OpenAIResponsesAPIHandler) forwardResponsesWebsocketInterrupt(ctx context.Context, sessionID string, payload []byte) error {
	responseID := gjson.GetBytes(payload, "response_id")
	if responseID.Type != gjson.String || strings.TrimSpace(responseID.String()) == "" {
		return fmt.Errorf("response.interrupt requires response_id")
	}
	if h == nil || h.AuthManager == nil {
		return cliproxyexecutor.ErrNoActiveUpstreamWebsocket
	}
	exec, ok := h.AuthManager.Executor("codex")
	if !ok || exec == nil {
		return cliproxyexecutor.ErrNoActiveUpstreamWebsocket
	}
	sender, ok := exec.(interface {
		InterruptExecutionSession(context.Context, string, []byte) error
	})
	if !ok || sender == nil {
		return fmt.Errorf("upstream executor does not support response.interrupt")
	}
	ctx = cliproxyexecutor.WithWebsocketAuthCheck(ctx, func(authID string) bool {
		// Home runtime credentials live on the execution session, not in the global map.
		auth, found := h.AuthManager.GetByID(authID)
		if !found || auth == nil {
			auth, found = h.AuthManager.GetExecutionSessionAuthByID(sessionID, authID)
		}
		return found && auth != nil && !auth.Disabled && auth.Status != coreauth.StatusDisabled
	})
	return sender.InterruptExecutionSession(ctx, sessionID, payload)
}
