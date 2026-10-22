package management

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
)

const (
	plusOAuthMaxSessions      = 16
	plusOAuthSessionTTL       = 30 * time.Minute
	plusOAuthCompletedKeep    = time.Minute
	plusOAuthMaxPromptBytes   = 512
	plusOAuthMaxInputBytes    = 64 * 1024
	plusOAuthStateBytes       = 32
	plusOAuthStatusPending    = "wait"
	plusOAuthStatusChallenge  = "challenge"
	plusOAuthStatusInput      = "input"
	plusOAuthStatusCommitting = "committing"
	plusOAuthStatusOK         = "ok"
	plusOAuthStatusError      = "error"
	plusOAuthStatusCancelled  = "cancelled"
)

// Public error categories. Provider/authenticator error text is never exposed
// or logged verbatim because it can carry tokens, authorization codes, or
// provider response bodies.
const (
	plusOAuthErrorAuthFailed = "authentication failed"
	plusOAuthErrorCancelled  = "oauth session was cancelled"
	plusOAuthErrorSaveFailed = "failed to save authentication tokens"
	plusOAuthErrorExpired    = "oauth session expired"
)

var (
	errPlusOAuthTooManySessions = errors.New("too many active oauth sessions")
	errPlusOAuthUnknownSession  = errors.New("unknown oauth session")
	errPlusOAuthStalePrompt     = errors.New("stale prompt response")
	errPlusOAuthNotAwaiting     = errors.New("oauth session is not awaiting input")
	errPlusOAuthClosed          = errors.New("oauth coordinator is closed")
)

// plusOAuthSession is one management-driven Plus provider login. All mutable
// state is guarded by mu; the login context is never held under the global
// session store lock.
type plusOAuthSession struct {
	state    string
	provider string

	loginCtx context.Context
	cancel   context.CancelFunc
	retire   func()

	mu          sync.Mutex
	status      string
	challenge   sdkAuth.LoginChallenge
	prompt      string
	promptID    string
	promptInput chan string
	errorText   string
	expireTimer *time.Timer
	// retireTimerSet is guarded by plusOAuthSessionStore.mu; it deduplicates
	// the single bounded retention removal per terminal session.
	retireTimerSet bool
}

func (s *plusOAuthSession) publicStatus() string {
	switch s.status {
	case plusOAuthStatusPending, plusOAuthStatusChallenge, plusOAuthStatusInput:
		return s.status
	case plusOAuthStatusOK:
		return plusOAuthStatusOK
	case plusOAuthStatusCancelled:
		return plusOAuthStatusError
	case plusOAuthStatusCommitting:
		return plusOAuthStatusPending
	default:
		return plusOAuthStatusError
	}
}

func (s *plusOAuthSession) payload() gin.H {
	s.mu.Lock()
	defer s.mu.Unlock()
	payload := gin.H{
		"status":   s.publicStatus(),
		"state":    s.state,
		"provider": s.provider,
	}
	if s.challenge.URL != "" {
		payload["url"] = s.challenge.URL
	}
	if s.challenge.UserCode != "" {
		payload["user_code"] = s.challenge.UserCode
	}
	if s.challenge.VerificationURI != "" {
		payload["verification_uri"] = s.challenge.VerificationURI
	}
	if s.prompt != "" {
		payload["prompt"] = s.prompt
	}
	if s.promptID != "" {
		payload["prompt_id"] = s.promptID
	}
	if s.errorText != "" {
		payload["error"] = s.errorText
	}
	return payload
}

// setChallenge records a device or browser challenge from the authenticator.
// The session lifetime is a fixed TTL and is never extended by provider
// activity, so expiry stays a single race-free deadline.
func (s *plusOAuthSession) setChallenge(challenge sdkAuth.LoginChallenge) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.isTerminalLocked() || s.status == plusOAuthStatusCommitting {
		return
	}
	s.challenge = challenge
	s.status = plusOAuthStatusChallenge
}

// requestInput blocks the authenticator until the operator submits a response.
func (s *plusOAuthSession) requestInput(ctx context.Context, prompt string) (string, error) {
	promptID, errPromptID := newPlusOAuthPromptID()
	if errPromptID != nil {
		return "", errPromptID
	}
	inputCh := make(chan string, 1)

	s.mu.Lock()
	if s.isTerminalLocked() || s.status == plusOAuthStatusCommitting {
		s.mu.Unlock()
		return "", context.Canceled
	}
	s.prompt = sanitizePlusOAuthPrompt(prompt)
	s.promptID = promptID
	s.promptInput = inputCh
	s.status = plusOAuthStatusInput
	s.mu.Unlock()

	select {
	case value := <-inputCh:
		return value, nil
	case <-ctx.Done():
		return "", ctx.Err()
	case <-s.loginCtx.Done():
		return "", s.loginCtx.Err()
	}
}

// deliverInput routes one response to the currently active prompt.
func (s *plusOAuthSession) deliverInput(promptID, input string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status != plusOAuthStatusInput || s.promptInput == nil {
		return errPlusOAuthNotAwaiting
	}
	if promptID != s.promptID {
		return errPlusOAuthStalePrompt
	}
	inputCh := s.promptInput
	s.promptInput = nil
	s.promptID = ""
	s.prompt = ""
	s.status = plusOAuthStatusPending
	inputCh <- input
	return nil
}

// beginCommit atomically moves to committing so cancellation can be refused
// while credentials are being persisted.
func (s *plusOAuthSession) beginCommit() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.isTerminalLocked() || s.status == plusOAuthStatusCommitting {
		return false
	}
	s.status = plusOAuthStatusCommitting
	s.stopExpireLocked()
	return true
}

// complete finalizes a successful credential write.
func (s *plusOAuthSession) complete() {
	s.mu.Lock()
	if s.status != plusOAuthStatusCommitting {
		s.mu.Unlock()
		return
	}
	s.status = plusOAuthStatusOK
	s.promptInput = nil
	s.challenge = sdkAuth.LoginChallenge{}
	s.mu.Unlock()
	s.cancel()
	s.scheduleRetirement()
}

// fail finalizes a login that never reached the commit fence.
func (s *plusOAuthSession) fail(message string) {
	s.mu.Lock()
	if s.isTerminalLocked() || s.status == plusOAuthStatusCommitting {
		s.mu.Unlock()
		return
	}
	s.status = plusOAuthStatusError
	s.errorText = sanitizeOAuthErrorText(message)
	s.promptInput = nil
	s.challenge = sdkAuth.LoginChallenge{}
	s.stopExpireLocked()
	s.mu.Unlock()
	s.cancel()
	s.scheduleRetirement()
}

// commitFailed finalizes a login whose credential write failed after
// beginCommit. Without this transition a failed save would leave the session
// stuck in committing forever (expiry stopped, cancellation refused).
func (s *plusOAuthSession) commitFailed(message string) {
	s.mu.Lock()
	if s.status != plusOAuthStatusCommitting {
		s.mu.Unlock()
		return
	}
	s.status = plusOAuthStatusError
	s.errorText = sanitizeOAuthErrorText(message)
	s.promptInput = nil
	s.challenge = sdkAuth.LoginChallenge{}
	s.stopExpireLocked()
	s.mu.Unlock()
	s.cancel()
	s.scheduleRetirement()
}

func (s *plusOAuthSession) isTerminalLocked() bool {
	return s.status == plusOAuthStatusOK || s.status == plusOAuthStatusError || s.status == plusOAuthStatusCancelled
}

func (s *plusOAuthSession) stopExpireLocked() {
	if s.expireTimer != nil {
		s.expireTimer.Stop()
		s.expireTimer = nil
	}
}

func (s *plusOAuthSession) scheduleRetirement() {
	if s.retire != nil {
		s.retire()
	}
}

// plusOAuthSessionStore bounds concurrent logins and retires finished sessions
// after a short retention window.
type plusOAuthSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*plusOAuthSession
	closed   bool
	// afterFunc schedules expiry and retention timers. It is fixed at
	// construction so async completion paths never read mutable test seams.
	afterFunc func(time.Duration, func()) *time.Timer
}

func newPlusOAuthSessionStore() *plusOAuthSessionStore {
	return newPlusOAuthSessionStoreWithAfterFunc(time.AfterFunc)
}

// newPlusOAuthSessionStoreWithAfterFunc builds a store with a deterministic
// timer implementation for tests; production stores use time.AfterFunc.
func newPlusOAuthSessionStoreWithAfterFunc(afterFunc func(time.Duration, func()) *time.Timer) *plusOAuthSessionStore {
	if afterFunc == nil {
		afterFunc = time.AfterFunc
	}
	return &plusOAuthSessionStore{sessions: make(map[string]*plusOAuthSession), afterFunc: afterFunc}
}

func (st *plusOAuthSessionStore) createSession(provider string, baseCtx context.Context) (*plusOAuthSession, error) {
	state, errState := newPlusOAuthState()
	if errState != nil {
		return nil, errState
	}
	loginCtx, cancel := context.WithCancel(baseCtx)

	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		cancel()
		return nil, errPlusOAuthClosed
	}
	if st.activeCountLocked() >= plusOAuthMaxSessions {
		st.mu.Unlock()
		cancel()
		return nil, errPlusOAuthTooManySessions
	}
	session := &plusOAuthSession{
		state:    state,
		provider: provider,
		loginCtx: loginCtx,
		cancel:   cancel,
		status:   plusOAuthStatusPending,
	}
	session.retire = func() { st.scheduleRetirement(state) }
	session.expireTimer = st.afterFunc(plusOAuthSessionTTL, func() { st.expire(state) })
	st.sessions[state] = session
	st.mu.Unlock()
	return session, nil
}

// activeCountLocked counts sessions that have not reached a terminal state.
// Retained terminal sessions do not consume the concurrency budget.
func (st *plusOAuthSessionStore) activeCountLocked() int {
	active := 0
	for _, session := range st.sessions {
		session.mu.Lock()
		terminal := session.isTerminalLocked()
		session.mu.Unlock()
		if !terminal {
			active++
		}
	}
	return active
}

func (st *plusOAuthSessionStore) get(state string) (*plusOAuthSession, bool) {
	if state == "" {
		return nil, false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	session, ok := st.sessions[state]
	return session, ok
}

func (st *plusOAuthSessionStore) status(state string) (gin.H, bool) {
	session, ok := st.get(state)
	if !ok {
		return nil, false
	}
	return session.payload(), true
}

// cancel returns (cancelled, committing, known). A session that is committing
// credentials cannot be cancelled and reports committing=true.
func (st *plusOAuthSessionStore) cancel(state string) (bool, bool, bool) {
	session, ok := st.get(state)
	if !ok {
		return false, false, false
	}
	session.mu.Lock()
	if session.status == plusOAuthStatusCommitting {
		session.mu.Unlock()
		return false, true, true
	}
	if session.isTerminalLocked() {
		session.mu.Unlock()
		return false, false, true
	}
	session.status = plusOAuthStatusCancelled
	session.promptInput = nil
	session.challenge = sdkAuth.LoginChallenge{}
	session.stopExpireLocked()
	session.mu.Unlock()
	session.cancel()
	session.scheduleRetirement()
	return true, false, true
}

func (st *plusOAuthSessionStore) deliverInput(state, promptID, input string) error {
	session, ok := st.get(state)
	if !ok {
		return errPlusOAuthUnknownSession
	}
	return session.deliverInput(strings.TrimSpace(promptID), input)
}

func (st *plusOAuthSessionStore) remove(state string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	session, ok := st.sessions[state]
	if !ok {
		return
	}
	delete(st.sessions, state)
	if session.expireTimer != nil {
		session.expireTimer.Stop()
		session.expireTimer = nil
	}
	session.cancel()
}

// scheduleRetirement schedules the single bounded retention removal for a
// terminal session. Repeated terminal transitions share one timer.
func (st *plusOAuthSessionStore) scheduleRetirement(state string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	session, ok := st.sessions[state]
	if !ok || session.retireTimerSet {
		return
	}
	session.retireTimerSet = true
	st.afterFunc(plusOAuthCompletedKeep, func() { st.remove(state) })
}

func (st *plusOAuthSessionStore) expire(state string) {
	session, ok := st.get(state)
	if !ok {
		return
	}
	session.mu.Lock()
	if session.isTerminalLocked() || session.status == plusOAuthStatusCommitting {
		session.mu.Unlock()
		return
	}
	session.status = plusOAuthStatusError
	session.errorText = plusOAuthErrorExpired
	session.promptInput = nil
	session.challenge = sdkAuth.LoginChallenge{}
	session.stopExpireLocked()
	session.mu.Unlock()
	session.cancel()
	session.scheduleRetirement()
}

// close fences the coordinator: future starts fail, pending logins are
// cancelled, and retained sessions are dropped. A credential write that is
// already committing may still finish; close does not roll it back.
func (st *plusOAuthSessionStore) close() {
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return
	}
	st.closed = true
	sessions := st.sessions
	st.sessions = make(map[string]*plusOAuthSession)
	st.mu.Unlock()

	for _, session := range sessions {
		session.mu.Lock()
		if session.isTerminalLocked() || session.status == plusOAuthStatusCommitting {
			session.stopExpireLocked()
			session.mu.Unlock()
			continue
		}
		session.status = plusOAuthStatusCancelled
		session.promptInput = nil
		session.challenge = sdkAuth.LoginChallenge{}
		session.stopExpireLocked()
		session.mu.Unlock()
		session.cancel()
	}
}

// cancelPending cancels logins that have not reached the commit fence while
// keeping the coordinator usable (for config reload / management disable).
func (st *plusOAuthSessionStore) cancelPending() int {
	st.mu.Lock()
	pending := make([]*plusOAuthSession, 0)
	for _, session := range st.sessions {
		session.mu.Lock()
		if session.isTerminalLocked() || session.status == plusOAuthStatusCommitting {
			session.mu.Unlock()
			continue
		}
		session.status = plusOAuthStatusCancelled
		session.promptInput = nil
		session.challenge = sdkAuth.LoginChallenge{}
		session.stopExpireLocked()
		session.mu.Unlock()
		pending = append(pending, session)
	}
	st.mu.Unlock()

	for _, session := range pending {
		session.cancel()
		session.scheduleRetirement()
	}
	return len(pending)
}

func newPlusOAuthState() (string, error) {
	raw := make([]byte, plusOAuthStateBytes)
	if _, errRead := rand.Read(raw); errRead != nil {
		return "", fmt.Errorf("generate oauth state: %w", errRead)
	}
	return hex.EncodeToString(raw), nil
}

func newPlusOAuthPromptID() (string, error) {
	raw := make([]byte, 16)
	if _, errRead := rand.Read(raw); errRead != nil {
		return "", fmt.Errorf("generate prompt id: %w", errRead)
	}
	return hex.EncodeToString(raw), nil
}

// sanitizeOAuthErrorText keeps the fixed public categories single-line and
// bounded. It is never fed provider error text.
func sanitizeOAuthErrorText(text string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case r < 0x20 || r == 0x7f:
			return -1
		default:
			return r
		}
	}, strings.TrimSpace(text))
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	if cleaned == "" {
		return plusOAuthErrorAuthFailed
	}
	if len(cleaned) > 300 {
		cleaned = cleaned[:300]
	}
	return cleaned
}

// sanitizePlusOAuthPrompt bounds prompt text shown in the web UI.
func sanitizePlusOAuthPrompt(prompt string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case r < 0x20 || r == 0x7f:
			return -1
		default:
			return r
		}
	}, strings.TrimSpace(prompt))
	if len(cleaned) > plusOAuthMaxPromptBytes {
		cleaned = cleaned[:plusOAuthMaxPromptBytes]
	}
	return cleaned
}
