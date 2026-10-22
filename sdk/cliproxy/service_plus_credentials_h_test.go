package cliproxy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	btauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/bt"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type plusCredentialSpyStore struct {
	mu    sync.Mutex
	saves []string
}

func (s *plusCredentialSpyStore) List(context.Context) ([]*coreauth.Auth, error) { return nil, nil }

func (s *plusCredentialSpyStore) Save(_ context.Context, auth *coreauth.Auth) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if auth != nil {
		s.saves = append(s.saves, auth.ID)
	}
	return "", nil
}

func (s *plusCredentialSpyStore) Delete(context.Context, string) error { return nil }

func (s *plusCredentialSpyStore) saveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.saves)
}

// H1: a stale add parked at publication must not land after the activation that
// removed its config entry has already deleted the credential.
func TestPlusCredentialStaleAddCannotResurrectRemovedCredential(t *testing.T) {
	originalLogin := plusBTLogin

	loginStarted := make(chan struct{}, 1)
	releaseLogin := make(chan struct{})
	plusBTLogin = func(context.Context, string, string, string) (*btauth.BTTokenStorage, error) {
		select {
		case loginStarted <- struct{}{}:
		default:
		}
		<-releaseLogin
		return &btauth.BTTokenStorage{UID: "uid-1", AccessKey: "ak-1", ServerID: "sid-1", Type: "bt"}, nil
	}

	entry := config.BTKey{Phone: "13800000000", Password: "cGFzcw=="}
	id, _ := btConfigAuthIdentity(entry)
	svc := &Service{coreManager: coreauth.NewManager(nil, nil, nil)}
	defer func() {
		svc.stopPlusCredentialActivation()
		plusBTLogin = originalLogin
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	svc.startPlusCredentialActivation(ctx, &config.Config{BTKey: []config.BTKey{entry}})
	select {
	case <-loginStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("BT login did not start")
	}

	parked := make(chan struct{}, 1)
	releaseParked := make(chan struct{})
	plusCredentialUpdateHook = func(update watcher.AuthUpdate) {
		if update.Action == watcher.AuthUpdateActionAdd && update.ID == id {
			select {
			case parked <- struct{}{}:
			default:
			}
			<-releaseParked
		}
	}
	// LIFO order: the releases run first, then stop joins the delete worker and
	// activation, then the test-global seam is cleared. This keeps failure paths
	// from wedging the join on a parked publication.
	defer func() { plusCredentialUpdateHook = nil }()
	defer svc.stopPlusCredentialActivation()
	defer closeOnce(releaseParked)
	defer closeOnce(releaseLogin)

	close(releaseLogin)
	select {
	case <-parked:
	case <-time.After(3 * time.Second):
		t.Fatal("pending add was not parked at publication")
	}

	// The config entry disappears while the stale add is parked.
	svc.startPlusCredentialActivation(ctx, &config.Config{})
	close(releaseParked)
	// Joining the activator drains the delete worker and the fenced activation;
	// the stale add can only have been discarded, never applied after the delete.
	svc.plusCredentialWG.Wait()
	if _, ok := svc.coreManager.GetByID(id); ok {
		t.Fatal("stale add resurrected a credential removed from config")
	}
}
func TestPlusCredentialActivationFencedAfterStop(t *testing.T) {
	originalLogin := plusBTLogin
	plusBTLogin = func(context.Context, string, string, string) (*btauth.BTTokenStorage, error) {
		return &btauth.BTTokenStorage{UID: "uid-first", AccessKey: "ak-first", ServerID: "sid", Type: "bt"}, nil
	}
	defer func() { plusBTLogin = originalLogin }()

	svc := &Service{coreManager: coreauth.NewManager(nil, nil, nil)}
	ctx := context.Background()
	first := config.BTKey{Phone: "13800000000", Password: "cGFzcw=="}
	svc.startPlusCredentialActivation(ctx, &config.Config{BTKey: []config.BTKey{first}})
	svc.stopPlusCredentialActivation()

	second := config.BTKey{Phone: "13900000000", Password: "cGFzcw=="}
	secondID, _ := btConfigAuthIdentity(second)
	svc.startPlusCredentialActivation(ctx, &config.Config{BTKey: []config.BTKey{second}})
	// start refuses synchronously after the fence, so no activation goroutine was
	// spawned and the manager state is final without polling.
	if _, ok := svc.coreManager.GetByID(secondID); ok {
		t.Fatal("activation started after stop registered a credential")
	}
}
func TestConfigManagedCredentialsSkipAuthStorePersistence(t *testing.T) {
	store := &plusCredentialSpyStore{}
	manager := coreauth.NewManager(store, nil, nil)
	background := context.Background()

	kiroAuth := &coreauth.Auth{
		ID:         "kiro:config:abc",
		Provider:   "kiro",
		Attributes: map[string]string{"source": "config:kiro[0]"},
		Metadata:   map[string]any{"type": "kiro", "access_token": "old", "refresh_token": "rt", "client_secret": "cs"},
	}
	if _, err := manager.Register(background, kiroAuth); err != nil {
		t.Fatalf("register config kiro auth: %v", err)
	}
	kiroAuth.StatusMessage = "quota exhausted"
	if _, err := manager.Update(background, kiroAuth); err != nil {
		t.Fatalf("status update config kiro auth: %v", err)
	}
	if got := store.saveCount(); got != 0 {
		t.Fatalf("config-managed kiro credential wrote to the auth store %d time(s)", got)
	}

	btAuth := &coreauth.Auth{
		ID:         "bt:config:def",
		Provider:   "bt",
		Attributes: map[string]string{"source": "config:bt[0]"},
		Metadata:   map[string]any{"type": "bt", "uid": "1", "access_key": "ak"},
	}
	if _, err := manager.Register(background, btAuth); err != nil {
		t.Fatalf("register config bt auth: %v", err)
	}
	if got := store.saveCount(); got != 0 {
		t.Fatalf("config-managed bt credential wrote to the auth store %d time(s)", got)
	}

	// File-backed credentials keep persisting: the predicate must stay targeted.
	fileAuth := &coreauth.Auth{
		ID:         "file-kiro",
		Provider:   "kiro",
		Attributes: map[string]string{"path": "/tmp/file-kiro.json"},
		Metadata:   map[string]any{"type": "kiro"},
	}
	if _, err := manager.Register(background, fileAuth); err != nil {
		t.Fatalf("register file kiro auth: %v", err)
	}
	if got := store.saveCount(); got == 0 {
		t.Fatal("file-backed kiro credential no longer persists")
	}
}

// H4: a presentation-only reload keeps the runtime token material (including a
// rotation the proxy already performed), while a token-file change replaces it.
func TestKiroReconcileKeepsRotatedTokensAndAcceptsTokenFileChanges(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "kiro.json")
	if err := os.WriteFile(tokenPath, []byte(`{"accessToken":"file-access-1","refreshToken":"file-refresh-1","expiresAt":"2030-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	newConfig := func(proxyURL string) *config.Config {
		return &config.Config{KiroKey: []config.KiroKey{{
			TokenFile: tokenPath,
			ProxyURL:  proxyURL,
		}}}
	}

	desiredList := synthesizeKiroConfigAuths(newConfig(""))
	if len(desiredList) != 1 {
		t.Fatalf("synthesized %d kiro auths, want 1", len(desiredList))
	}
	desired := desiredList[0]
	if desired.Attributes["kiro_config_fingerprint"] == "" || desired.Attributes["kiro_token_fingerprint"] == "" {
		t.Fatalf("synthesizer did not stamp fingerprints: %v", desired.Attributes)
	}

	existing := desired.Clone()
	existing.Metadata["access_token"] = "runtime-rotated-access"
	existing.Metadata["refresh_token"] = "runtime-rotated-refresh"
	existing.Metadata["expires_at"] = "2031-01-01T00:00:00Z"

	// Presentation-only change: same fingerprints, runtime tokens preserved.
	presentation := synthesizeKiroConfigAuths(newConfig("http://127.0.0.1:7890"))
	if len(presentation) != 1 {
		t.Fatalf("synthesized %d presentation auths, want 1", len(presentation))
	}
	merged, publish, refresh := reconcileKiroConfigAuth(existing, presentation[0])
	if !publish || refresh {
		t.Fatalf("presentation reconcile publish=%v refresh=%v, want publish only", publish, refresh)
	}
	if merged.Metadata["access_token"] != "runtime-rotated-access" || merged.Metadata["refresh_token"] != "runtime-rotated-refresh" {
		t.Fatalf("presentation reload rolled back rotated tokens: %v", merged.Metadata)
	}
	if merged.ProxyURL != "http://127.0.0.1:7890" {
		t.Fatalf("presentation change not applied: %q", merged.ProxyURL)
	}

	// Token-file change: new material must replace the runtime tokens.
	if err := os.WriteFile(tokenPath, []byte(`{"accessToken":"file-access-2","refreshToken":"file-refresh-2","expiresAt":"2030-06-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatalf("rewrite token file: %v", err)
	}
	changed := synthesizeKiroConfigAuths(newConfig(""))
	if len(changed) != 1 {
		t.Fatalf("synthesized %d changed auths, want 1", len(changed))
	}
	mergedChanged, publishChanged, _ := reconcileKiroConfigAuth(existing, changed[0])
	if !publishChanged {
		t.Fatal("token-file change was not republished")
	}
	if mergedChanged.Metadata["access_token"] != "file-access-2" {
		t.Fatalf("token-file change kept stale runtime access token: %v", mergedChanged.Metadata["access_token"])
	}
}

// Bounded login backoff: after a failed login the same credential is not retried
// immediately on every config reload.
func TestBTLoginBackoffBoundsRepeatedFailures(t *testing.T) {
	originalLogin := plusBTLogin
	attempts := make(chan int, 8)
	var loginCount int
	var countMu sync.Mutex
	plusBTLogin = func(context.Context, string, string, string) (*btauth.BTTokenStorage, error) {
		countMu.Lock()
		loginCount++
		current := loginCount
		countMu.Unlock()
		select {
		case attempts <- current:
		default:
		}
		return nil, errors.New("invalid credentials")
	}

	entry := config.BTKey{Phone: "13800000000", Password: "cGFzcw=="}
	svc := &Service{coreManager: coreauth.NewManager(nil, nil, nil)}
	defer func() {
		svc.stopPlusCredentialActivation()
		plusBTLogin = originalLogin
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	svc.startPlusCredentialActivation(ctx, &config.Config{BTKey: []config.BTKey{entry}})
	select {
	case <-attempts:
	case <-time.After(3 * time.Second):
		t.Fatal("initial login was not attempted")
	}
	svc.plusCredentialWG.Wait()
	if _, ok := svc.btLoginFailures[btConfigAuthID(entry)]; !ok {
		t.Fatalf("failure was not recorded: %v", svc.btLoginFailures)
	}

	// A reload during the backoff window must skip the login entirely.
	svc.startPlusCredentialActivation(ctx, &config.Config{BTKey: []config.BTKey{entry}})
	svc.plusCredentialWG.Wait()
	select {
	case got := <-attempts:
		t.Fatalf("login retried during backoff, attempt %d", got)
	default:
	}

	// Once the delay elapses a reload may retry.
	id := btConfigAuthID(entry)
	svc.plusCredentialMu.Lock()
	failure := svc.btLoginFailures[id]
	failure.nextAttempt = time.Now().Add(-time.Second)
	svc.btLoginFailures[id] = failure
	svc.plusCredentialMu.Unlock()
	svc.startPlusCredentialActivation(ctx, &config.Config{BTKey: []config.BTKey{entry}})
	select {
	case <-attempts:
	case <-time.After(3 * time.Second):
		t.Fatal("login was not retried after the backoff delay")
	}
	svc.plusCredentialWG.Wait()
}
func btConfigAuthID(entry config.BTKey) string {
	id, _ := btConfigAuthIdentity(entry)
	return id
}

func TestBTMaskPhoneKeepsOnlyLastDigits(t *testing.T) {
	if got := btauth.MaskPhone("13800000000"); got != "*******0000" {
		t.Fatalf("MaskPhone = %q, want *******0000", got)
	}
	if got := btauth.MaskPhone("123"); got != "***" {
		t.Fatalf("MaskPhone short = %q, want ***", got)
	}
}

// TestPlusCredentialRapidReloadStillDeletesSupersededCredential guards a rapid
// A->B->C reload: the delete computed for A is parked until after the reload to
// C bumped the generation. It must still be published, because A is absent from
// the latest desired set; a pure generation fence would silently drop it and
// leave the manager serving a removed credential.
func TestPlusCredentialRapidReloadStillDeletesSupersededCredential(t *testing.T) {
	originalLogin := plusBTLogin
	plusBTLogin = func(ctx context.Context, phone, password, proxyURL string) (*btauth.BTTokenStorage, error) {
		return &btauth.BTTokenStorage{UID: "uid-" + phone, AccessKey: "ak-" + phone, ServerID: "sid", Type: "bt"}, nil
	}

	entryA := config.BTKey{Phone: "13800000001", Password: "pw"}
	entryB := config.BTKey{Phone: "13800000002", Password: "pw"}
	entryC := config.BTKey{Phone: "13800000003", Password: "pw"}
	idA, _ := btConfigAuthIdentity(entryA)
	idB, _ := btConfigAuthIdentity(entryB)
	idC, _ := btConfigAuthIdentity(entryC)

	svc := &Service{cfg: &config.Config{BTKey: []config.BTKey{entryA}}, coreManager: coreauth.NewManager(nil, nil, nil)}
	defer func() {
		svc.stopPlusCredentialActivation()
		plusCredentialUpdateHook = nil
		plusBTLogin = originalLogin
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.startPlusCredentialActivation(ctx, svc.cfg)
	svc.plusCredentialWG.Wait()
	if _, ok := svc.coreManager.GetByID(idA); !ok {
		t.Fatal("credential A was not registered")
	}

	parked := make(chan struct{})
	release := make(chan struct{})
	// Registered after the stop/join defer, so it runs first and never leaves the
	// hooked worker parked when an assertion fails.
	defer func() { closeOnce(release) }()
	plusCredentialUpdateHook = func(update watcher.AuthUpdate) {
		if update.ID != idA || update.Action != watcher.AuthUpdateActionDelete {
			return
		}
		select {
		case <-parked:
		default:
			close(parked)
		}
		<-release
	}

	doneB := make(chan struct{})
	go func() {
		defer close(doneB)
		svc.startPlusCredentialActivation(ctx, &config.Config{BTKey: []config.BTKey{entryB}})
	}()
	select {
	case <-parked:
	case <-time.After(3 * time.Second):
		t.Fatal("stale delete for A was not published")
	}

	// Reload to C while the delete for A is still parked. This bumps the
	// generation past the one that computed the delete.
	svc.startPlusCredentialActivation(ctx, &config.Config{BTKey: []config.BTKey{entryC}})
	close(release)
	<-doneB
	// The delete worker and every activation are part of the activator
	// WaitGroup, so draining it makes the final credential set deterministic.
	svc.plusCredentialWG.Wait()
	_, hasA := svc.coreManager.GetByID(idA)
	_, hasB := svc.coreManager.GetByID(idB)
	_, hasC := svc.coreManager.GetByID(idC)
	if hasA || hasB || !hasC {
		t.Fatalf("rapid reload left stale credentials: A=%v B=%v C=%v", hasA, hasB, hasC)
	}
}
func TestKiroTokenFileRemovalDeletesConfigCredential(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "kiro-token.json")
	body := `{"accessToken":"access-value","refreshToken":"refresh-value","profileArn":"arn:aws:codewhisperer:us-east-1:1:profile/test","region":"us-east-1"}`
	if err := os.WriteFile(tokenPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	cfg := &config.Config{KiroKey: []config.KiroKey{{TokenFile: tokenPath}}}
	synthesized := synthesizeKiroConfigAuths(cfg)
	if len(synthesized) != 1 {
		t.Fatalf("synthesized %d kiro auths, want 1", len(synthesized))
	}
	id := synthesized[0].ID

	svc := &Service{cfg: cfg, coreManager: coreauth.NewManager(nil, nil, nil)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.startPlusCredentialActivation(ctx, cfg)
	defer svc.stopPlusCredentialActivation()
	if _, ok := svc.waitForRegisteredPlusAuth(ctx, id, 5*time.Second); !ok {
		t.Fatal("kiro config credential was not registered")
	}

	if errRemove := os.Remove(tokenPath); errRemove != nil {
		t.Fatalf("remove token file: %v", errRemove)
	}

	// fsnotify delivery is genuinely asynchronous; a bounded ticker is the only
	// time-based primitive used for this event wait.
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("deleted token file left its config credential active")
		case <-ticker.C:
			if _, exists := svc.coreManager.GetByID(id); !exists {
				return
			}
		}
	}
}

// TestPlusCredentialDeleteHandoffJoinsStop proves the stale-delete handoff is
// tracked by the activator WaitGroup and cannot publish after stop has fenced
// the activator, even when it is blocked on the publication lock.
func TestPlusCredentialDeleteHandoffJoinsStop(t *testing.T) {
	id := "bt:config:review-stop"
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{ID: id, Provider: "bt", Status: coreauth.StatusActive, Metadata: map[string]any{"type": "bt"}}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	svc := &Service{coreManager: manager}
	svc.plusCredentialIDs = map[string]struct{}{}

	started := make(chan struct{})
	plusCredentialUpdateHook = func(update watcher.AuthUpdate) {
		select {
		case <-started:
		default:
			close(started)
		}
	}
	// Join the tracked worker before clearing the seam, so the hook is never
	// written while the worker can still read it.
	defer func() {
		svc.plusCredentialWG.Wait()
		plusCredentialUpdateHook = nil
	}()

	// Hold the publication lock so the worker parks inside the handoff.
	var unlockOnce sync.Once
	unlock := func() { unlockOnce.Do(svc.plusCredentialPublishMu.Unlock) }
	svc.plusCredentialPublishMu.Lock()
	// Registered after the join defer, so it runs before it and releases the
	// worker even when an earlier assertion fails.
	defer unlock()
	svc.enqueuePlusCredentialDelete(watcher.AuthUpdate{Action: watcher.AuthUpdateActionDelete, ID: id})
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("delete worker did not reach the publication handoff")
	}

	stopDone := make(chan struct{})
	go func() {
		svc.stopPlusCredentialActivation()
		close(stopDone)
	}()

	// Observe the fence under its mutex. The bounded ticker is only a scheduling
	// primitive for reading the flag; the causal assertion is that stop cannot
	// have returned while the worker is parked on the publication lock.
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	fenceDeadline := time.After(3 * time.Second)
	for {
		svc.plusCredentialMu.Lock()
		fenced := svc.plusCredentialStopped
		svc.plusCredentialMu.Unlock()
		if fenced {
			break
		}
		select {
		case <-fenceDeadline:
			t.Fatal("stop did not set the activation fence")
		case <-ticker.C:
		}
	}
	select {
	case <-stopDone:
		t.Fatal("stop returned while the delete worker held the publication lock")
	default:
	}

	unlock()
	select {
	case <-stopDone:
	case <-time.After(3 * time.Second):
		t.Fatal("stop did not join the delete worker")
	}
	if _, exists := manager.GetByID(id); !exists {
		t.Fatal("stale delete was applied after stop fenced the activator")
	}
}
func TestPlusCredentialDeleteHandoffIsBoundedAndDeduped(t *testing.T) {
	svc := &Service{coreManager: coreauth.NewManager(nil, nil, nil)}
	id := "bt:config:churn"
	first := make(chan struct{})
	release := make(chan struct{})
	var hookCalls int
	var hookMu sync.Mutex
	plusCredentialUpdateHook = func(update watcher.AuthUpdate) {
		hookMu.Lock()
		hookCalls++
		hookMu.Unlock()
		select {
		case <-first:
		default:
			close(first)
		}
		<-release
	}
	defer func() {
		// Join the tracked worker before clearing the seam; the worker may still
		// read the hook on a failure path.
		closeOnce(release)
		svc.plusCredentialWG.Wait()
		plusCredentialUpdateHook = nil
	}()

	update := watcher.AuthUpdate{Action: watcher.AuthUpdateActionDelete, ID: id}
	for i := 0; i < 20; i++ {
		svc.enqueuePlusCredentialDelete(update)
	}
	select {
	case <-first:
	case <-time.After(3 * time.Second):
		t.Fatal("delete worker never started")
	}
	for i := 0; i < 20; i++ {
		svc.enqueuePlusCredentialDelete(update)
	}
	hookMu.Lock()
	callsWhileInFlight := hookCalls
	hookMu.Unlock()
	if callsWhileInFlight != 1 {
		t.Fatalf("hook calls while delete in flight = %d, want 1 (deduplicated handoff)", callsWhileInFlight)
	}
	close(release)

	// The worker is tracked by plusCredentialWG; joining it is the deterministic
	// drain signal instead of polling its state.
	svc.plusCredentialWG.Wait()
	svc.plusCredentialMu.Lock()
	running := svc.plusCredentialDeleteWorker
	queued := len(svc.plusCredentialDeleteQueue)
	svc.plusCredentialMu.Unlock()
	if running || queued != 0 {
		t.Fatalf("delete worker state after join: running=%v queued=%d", running, queued)
	}
	hookMu.Lock()
	finalCalls := hookCalls
	hookMu.Unlock()
	if finalCalls != 1 {
		t.Fatalf("hook calls = %d, want 1", finalCalls)
	}
}

// closeOnce unblocks a parked worker from cleanup paths without panicking when
// the test body already closed the channel. Only the test goroutine calls it
// (defers run after the body), so the receive/close pair is race free.
func closeOnce(ch chan struct{}) {
	select {
	case <-ch:
	default:
		close(ch)
	}
}
