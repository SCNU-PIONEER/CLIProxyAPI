package cliproxy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	btauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/bt"
	kiroauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/kiro"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// plusBTLogin is the BaoTa credential acquisition seam. Production code uses
// btauth.LoginContext; tests replace it to avoid network access.
var plusBTLogin = btauth.LoginContext

// plusKiroForceRefresh is a test seam around the manager refresh path.
var plusKiroForceRefresh = func(ctx context.Context, s *Service, id string) (*coreauth.Auth, error) {
	return s.coreManager.ForceRefreshAuth(coreauth.WithSkipPersist(ctx), id)
}

const (
	plusCredentialKindKiro = "kiro"
	plusCredentialKindBT   = "bt"

	// plusKiroRegistrationWait bounds how long activation waits for the emitted
	// auth to become visible in the core manager before forcing a refresh.
	plusKiroRegistrationWait = 10 * time.Second
)

const (
	// btLoginBackoffBase is the first retry delay after a failed BaoTa login.
	btLoginBackoffBase = 30 * time.Second
	// btLoginBackoffMax caps the per-credential login retry delay.
	btLoginBackoffMax = 15 * time.Minute
	// btLoginBackoffShiftCap bounds the exponential shift so the delay stays
	// representable even after many consecutive failures.
	btLoginBackoffShiftCap = 5
)

// btLoginFailure tracks per-credential login backoff state.
type btLoginFailure struct {
	attempts    int
	nextAttempt time.Time
}

// plusCredentialUpdateHook is a test seam invoked before a credential update
// is fenced and published. Tests use it to park an in-flight add and prove a
// later delete cannot be overtaken by it.
var plusCredentialUpdateHook func(update watcher.AuthUpdate)

// startPlusCredentialActivation reconciles the config-driven Plus credentials
// that cannot be represented by pure synthesis (BaoTa accounts need a login
// round trip) and refresh-only Kiro entries. The snapshots are taken here under
// the caller's config lock; all network work happens in the returned goroutine.
func (s *Service) startPlusCredentialActivation(ctx context.Context, cfg *config.Config) {
	if s == nil || cfg == nil {
		return
	}
	watchPaths := kiroTokenFilePaths(cfg)
	// Subscribe to the token-file directories before snapshotting them: a
	// rewrite that lands right after activation returns must not fall between
	// the snapshot and a lazily-started watch. Events raised during the
	// snapshot are buffered by fsnotify and replayed once the loop starts.
	tokenWatcher := s.newKiroTokenWatcher(watchPaths)
	// The Kiro fingerprint configuration is process-wide; refresh it on every
	// applied config so a reload cannot leave stale identity values behind.
	kiroauth.InitFingerprintConfig(cfg)
	kiroAuths := synthesizeKiroConfigAuths(cfg)
	btEntries := append([]config.BTKey(nil), cfg.BTKey...)

	desired := make(map[string]struct{}, len(kiroAuths)+len(btEntries))
	for _, auth := range kiroAuths {
		desired[auth.ID] = struct{}{}
	}
	for i := range btEntries {
		id, _ := btConfigAuthIdentity(btEntries[i])
		desired[id] = struct{}{}
	}

	s.plusCredentialMu.Lock()
	if s.plusCredentialStopped {
		s.plusCredentialMu.Unlock()
		if tokenWatcher != nil {
			_ = tokenWatcher.Close()
		}
		return
	}
	if s.plusCredentialCancel != nil {
		s.plusCredentialCancel()
	}
	activationCtx, cancel := context.WithCancel(context.Background())
	s.plusCredentialCancel = cancel
	s.plusCredentialGeneration++
	generation := s.plusCredentialGeneration
	stale := make([]string, 0)
	for id := range s.plusCredentialIDs {
		if _, ok := desired[id]; !ok {
			stale = append(stale, id)
		}
	}
	s.plusCredentialIDs = desired
	s.plusCredentialWG.Add(1)
	if tokenWatcher != nil {
		s.plusCredentialWG.Add(1)
	}
	s.plusCredentialMu.Unlock()

	// Stale deletes are published on desired-set membership rather than on the
	// generation that computed them: a rapid A->B->C reload bumps the
	// generation again before an earlier delete is published, and the older
	// credential is still absent from the latest desired set.
	for _, id := range stale {
		s.enqueuePlusCredentialDelete(watcher.AuthUpdate{Action: watcher.AuthUpdateActionDelete, ID: id})
	}

	go func() {
		defer s.plusCredentialWG.Done()
		s.activatePlusCredentials(activationCtx, generation, kiroAuths, btEntries)
	}()
	if tokenWatcher != nil {
		go func() {
			defer s.plusCredentialWG.Done()
			s.watchKiroTokenFiles(activationCtx, generation, watchPaths, tokenWatcher)
		}()
	}
	_ = ctx
}

// stopPlusCredentialActivation cancels in-flight activation and waits for it.
func (s *Service) stopPlusCredentialActivation() {
	if s == nil {
		return
	}
	s.plusCredentialMu.Lock()
	s.plusCredentialStopped = true
	if s.plusCredentialCancel != nil {
		s.plusCredentialCancel()
		s.plusCredentialCancel = nil
	}
	s.plusCredentialMu.Unlock()
	s.plusCredentialWG.Wait()
}

func (s *Service) plusCredentialGenerationCurrent(generation uint64) bool {
	s.plusCredentialMu.Lock()
	defer s.plusCredentialMu.Unlock()
	return s.plusCredentialGeneration == generation
}

func (s *Service) activatePlusCredentials(ctx context.Context, generation uint64, kiroAuths []*coreauth.Auth, btEntries []config.BTKey) {
	if s == nil || s.coreManager == nil {
		return
	}
	s.activateKiroConfigAuths(ctx, generation, kiroAuths)
	s.activateBTConfigAuths(ctx, generation, btEntries)
}

func (s *Service) activateKiroConfigAuths(ctx context.Context, generation uint64, kiroAuths []*coreauth.Auth) {
	refreshOnly := make([]*coreauth.Auth, 0)
	for _, auth := range kiroAuths {
		if auth == nil {
			continue
		}
		if ctx.Err() != nil || !s.plusCredentialGenerationCurrent(generation) {
			return
		}
		existing, ok := s.coreManager.GetByID(auth.ID)
		if !ok {
			existing = nil
		}
		candidate, publish, needsRefresh := reconcileKiroConfigAuth(existing, auth)
		if publish && candidate != nil {
			s.publishPlusCredentialUpdate(ctx, generation, watcher.AuthUpdate{Action: watcher.AuthUpdateActionAdd, ID: candidate.ID, Auth: candidate.Clone()})
		}
		if needsRefresh {
			target := candidate
			if target == nil {
				target = auth
			}
			refreshOnly = append(refreshOnly, target)
		}
	}
	for _, auth := range refreshOnly {
		if ctx.Err() != nil || !s.plusCredentialGenerationCurrent(generation) {
			return
		}
		registered, ok := s.waitForRegisteredPlusAuth(ctx, auth.ID, plusKiroRegistrationWait)
		if !ok {
			log.Warnf("kiro config %s: credential did not register before refresh", auth.ID)
			continue
		}
		if kiroAuthAccessToken(registered) != "" {
			continue
		}
		refreshed, errRefresh := plusKiroForceRefresh(ctx, s, auth.ID)
		if errRefresh != nil {
			log.Warnf("kiro config %s: unable to acquire access token from refresh token: %v", auth.ID, errRefresh)
			continue
		}
		if refreshed == nil || kiroAuthAccessToken(refreshed) == "" {
			log.Warnf("kiro config %s: refresh finished without an access token", auth.ID)
		}
	}
}

func (s *Service) activateBTConfigAuths(ctx context.Context, generation uint64, entries []config.BTKey) {
	for i := range entries {
		if ctx.Err() != nil || !s.plusCredentialGenerationCurrent(generation) {
			return
		}
		entry := entries[i]
		id, _ := btConfigAuthIdentity(entry)
		if existing, ok := s.coreManager.GetByID(id); ok {
			token, hasToken := btConfigTokenFromAuth(existing)
			if hasToken {
				candidate := buildBTConfigAuth(entry, i, token)
				if btConfigAuthEquivalent(existing, candidate) {
					continue
				}
				s.publishPlusCredentialUpdate(ctx, generation, watcher.AuthUpdate{Action: watcher.AuthUpdateActionAdd, ID: id, Auth: candidate.Clone()})
				continue
			}
		}
		phone := strings.TrimSpace(entry.Phone)
		if phone == "" || strings.TrimSpace(entry.Password) == "" {
			log.Warnf("bt[%d]: missing phone or password, skipping", i)
			continue
		}
		if s.btLoginBackoffActive(id, time.Now()) {
			log.Debugf("bt[%d]: login for %s is backing off after a previous failure", i, btauth.MaskPhone(phone))
			continue
		}
		token, errLogin := plusBTLogin(ctx, phone, entry.Password, strings.TrimSpace(entry.ProxyURL))
		if errLogin != nil {
			s.recordBTLoginFailure(id)
			if ctx.Err() == nil {
				log.Warnf("bt[%d]: login failed for phone %s: %v", i, btauth.MaskPhone(phone), errLogin)
			}
			continue
		}
		if token == nil || strings.TrimSpace(token.UID) == "" {
			s.recordBTLoginFailure(id)
			log.Warnf("bt[%d]: login for phone %s returned no uid", i, btauth.MaskPhone(phone))
			continue
		}
		if ctx.Err() != nil || !s.plusCredentialGenerationCurrent(generation) {
			return
		}
		s.clearBTLoginFailure(id)
		auth := buildBTConfigAuth(entry, i, token)
		s.publishPlusCredentialUpdate(ctx, generation, watcher.AuthUpdate{Action: watcher.AuthUpdateActionAdd, ID: auth.ID, Auth: auth.Clone()})
	}
}

func (s *Service) waitForRegisteredPlusAuth(ctx context.Context, id string, timeout time.Duration) (*coreauth.Auth, bool) {
	if s == nil || s.coreManager == nil {
		return nil, false
	}
	deadline := time.Now().Add(timeout)
	for {
		if auth, ok := s.coreManager.GetByID(id); ok {
			return auth, true
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return nil, false
		}
		select {
		case <-ctx.Done():
			return nil, false
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// synthesizeKiroConfigAuths runs the pure config synthesizer and keeps only the
// Kiro entries it produced.
func synthesizeKiroConfigAuths(cfg *config.Config) []*coreauth.Auth {
	if cfg == nil {
		return nil
	}
	auths, errSynthesize := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{
		Config:      cfg,
		Now:         time.Now(),
		IDGenerator: synthesizer.NewStableIDGenerator(),
	})
	if errSynthesize != nil {
		log.Warnf("failed to synthesize kiro config auths: %v", errSynthesize)
		return nil
	}
	out := make([]*coreauth.Auth, 0, len(auths))
	for _, auth := range auths {
		if auth == nil || !strings.EqualFold(auth.Provider, plusCredentialKindKiro) {
			continue
		}
		if auth.AuthSourceKind() != coreauth.AuthSourceConfig {
			continue
		}
		out = append(out, auth)
	}
	return out
}

// btConfigAuthIdentity derives the stable runtime auth id and the stable token
// written into the auth source attribute. The password participates only via
// the generator's hash, never as plaintext.
func btConfigAuthIdentity(entry config.BTKey) (string, string) {
	idGen := synthesizer.NewStableIDGenerator()
	return idGen.Next(
		"bt:config",
		strings.TrimSpace(entry.Phone),
		entry.Password,
		strings.TrimSpace(entry.ProxyURL),
		strings.TrimSpace(entry.Prefix),
	)
}

func buildBTConfigAuth(entry config.BTKey, index int, token *btauth.BTTokenStorage) *coreauth.Auth {
	id, tokenID := btConfigAuthIdentity(entry)
	prefix := strings.TrimSpace(entry.Prefix)
	phone := strings.TrimSpace(entry.Phone)
	attrs := map[string]string{
		"source":       fmt.Sprintf("config:bt[%s]", tokenID),
		"config_index": strconv.Itoa(index),
		"bt_phone":     phone,
	}
	if entry.Priority != 0 {
		attrs["priority"] = strconv.Itoa(entry.Priority)
	}
	for headerKey, headerValue := range entry.Headers {
		key := strings.TrimSpace(headerKey)
		value := strings.TrimSpace(headerValue)
		if key == "" || value == "" {
			continue
		}
		attrs["header:"+key] = value
	}
	metadata := map[string]any{
		"type": "bt",
		// Config-managed BT tokens carry no expiry. A per-auth periodic
		// interval keeps the scheduler revisiting this credential; it is a
		// cadence, not a claimed expiry, and file-backed BT auths are unaffected.
		"refresh_interval_seconds": int64((6 * time.Hour) / time.Second),
	}
	if phone != "" {
		metadata["bt_phone"] = phone
	}
	if token != nil {
		metadata["uid"] = token.UID
		metadata["access_key"] = token.AccessKey
		metadata["serverid"] = token.ServerID
	}
	auth := &coreauth.Auth{
		ID:              id,
		Provider:        plusCredentialKindBT,
		Label:           phone,
		Prefix:          prefix,
		Status:          coreauth.StatusActive,
		ProxyURL:        strings.TrimSpace(entry.ProxyURL),
		Attributes:      attrs,
		Metadata:        metadata,
		CreatedAt:       time.Now(),
		LastRefreshedAt: time.Now(),
	}
	synthesizer.ApplyAuthExcludedModelsMeta(auth, entry.ExcludedModels, "")
	return auth
}

func btConfigTokenFromAuth(auth *coreauth.Auth) (*btauth.BTTokenStorage, bool) {
	if auth == nil || auth.Metadata == nil {
		return nil, false
	}
	uid := strings.TrimSpace(metaString(auth.Metadata["uid"]))
	accessKey := strings.TrimSpace(metaString(auth.Metadata["access_key"]))
	serverID := strings.TrimSpace(metaString(auth.Metadata["serverid"]))
	if uid == "" || accessKey == "" {
		return nil, false
	}
	return &btauth.BTTokenStorage{UID: uid, AccessKey: accessKey, ServerID: serverID, Type: "bt"}, true
}

func btConfigAuthEquivalent(existing, candidate *coreauth.Auth) bool {
	if existing == nil || candidate == nil {
		return false
	}
	if existing.Prefix != candidate.Prefix || existing.ProxyURL != candidate.ProxyURL {
		return false
	}
	if !stringMapEqual(existing.Attributes, candidate.Attributes) {
		return false
	}
	return metaValueEqual(existing.Metadata["uid"], candidate.Metadata["uid"]) &&
		metaValueEqual(existing.Metadata["access_key"], candidate.Metadata["access_key"]) &&
		metaValueEqual(existing.Metadata["serverid"], candidate.Metadata["serverid"])
}

// publishPlusCredentialUpdate serializes credential publications and fences
// them on the activation generation. Serialization plus per-credential queue
// ordering guarantees a stale add can never be applied after the delete that
// superseded it.
func (s *Service) publishPlusCredentialUpdate(ctx context.Context, generation uint64, update watcher.AuthUpdate) {
	if s == nil {
		return
	}
	if hook := plusCredentialUpdateHook; hook != nil {
		hook(update)
	}
	s.plusCredentialPublishMu.Lock()
	defer s.plusCredentialPublishMu.Unlock()
	s.plusCredentialMu.Lock()
	allowed := !s.plusCredentialStopped && s.plusCredentialGeneration == generation
	s.plusCredentialMu.Unlock()
	if !allowed {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return
	}
	s.dispatchPlusCredentialUpdate(ctx, update)
}

// enqueuePlusCredentialDelete schedules a stale-entry delete from the config
// apply path without blocking on publication locks, queues or network. Deletes
// are deduplicated by auth ID and drained by a single tracked worker so churn
// cannot spawn a goroutine per removal; the worker is registered on the
// activator WaitGroup before it starts, and it re-checks stopped/desired state
// under the publication lock, so it can never write after stop has fenced the
// activator. The desired-set check is also enforced again at the consumer.
func (s *Service) enqueuePlusCredentialDelete(update watcher.AuthUpdate) {
	if s == nil {
		return
	}
	s.plusCredentialMu.Lock()
	if !s.plusCredentialDeleteEnqueueLocked(update) {
		s.plusCredentialMu.Unlock()
		return
	}
	s.plusCredentialMu.Unlock()
}

func (s *Service) plusCredentialDeleteEnqueueLocked(update watcher.AuthUpdate) bool {
	if s.plusCredentialStopped {
		return false
	}
	if _, stillDesired := s.plusCredentialIDs[update.ID]; stillDesired {
		return false
	}
	if s.plusCredentialDeletePending == nil {
		s.plusCredentialDeletePending = make(map[string]struct{})
	}
	if _, duplicated := s.plusCredentialDeletePending[update.ID]; duplicated {
		return false
	}
	s.plusCredentialDeletePending[update.ID] = struct{}{}
	s.plusCredentialDeleteQueue = append(s.plusCredentialDeleteQueue, update)
	if !s.plusCredentialDeleteWorker {
		s.plusCredentialDeleteWorker = true
		s.plusCredentialWG.Add(1)
		go s.runPlusCredentialDeleteWorker()
	}
	return true
}

// runPlusCredentialDeleteWorker drains the pending-delete queue serially. It is
// the only goroutine that publishes stale deletes, so publication order matches
// enqueue order and the number of background goroutines stays bounded by one.
func (s *Service) runPlusCredentialDeleteWorker() {
	defer s.plusCredentialWG.Done()
	for {
		s.plusCredentialMu.Lock()
		if s.plusCredentialStopped || len(s.plusCredentialDeleteQueue) == 0 {
			s.plusCredentialDeleteQueue = nil
			s.plusCredentialDeletePending = nil
			s.plusCredentialDeleteWorker = false
			s.plusCredentialMu.Unlock()
			return
		}
		update := s.plusCredentialDeleteQueue[0]
		s.plusCredentialDeleteQueue = s.plusCredentialDeleteQueue[1:]
		s.plusCredentialMu.Unlock()

		if hook := plusCredentialUpdateHook; hook != nil {
			hook(update)
		}
		s.publishQueuedPlusCredentialDelete(update)

		s.plusCredentialMu.Lock()
		delete(s.plusCredentialDeletePending, update.ID)
		s.plusCredentialMu.Unlock()
	}
}

// publishQueuedPlusCredentialDelete applies one queued delete under the
// publication lock. It deliberately blocks on that lock (the caller is the
// dedicated worker, not the config apply path) and re-validates the state after
// acquiring it.
func (s *Service) publishQueuedPlusCredentialDelete(update watcher.AuthUpdate) {
	s.plusCredentialPublishMu.Lock()
	defer s.plusCredentialPublishMu.Unlock()
	s.plusCredentialMu.Lock()
	allowed := !s.plusCredentialStopped
	if allowed {
		_, stillDesired := s.plusCredentialIDs[update.ID]
		allowed = !stillDesired
	}
	s.plusCredentialMu.Unlock()
	if !allowed {
		return
	}
	s.dispatchPlusCredentialDelete(update)
}

// dispatchPlusCredentialDelete hands a queued stale delete to the ordered
// pipelines without blocking the worker forever: it drops the update into the
// service queue when there is room, otherwise it applies it inline (the delete
// path is cheap and the consumer fence drops deletes whose ID is desired again).
func (s *Service) dispatchPlusCredentialDelete(update watcher.AuthUpdate) {
	if watcherInstance := s.currentWatcher(); watcherInstance != nil && watcherInstance.DispatchRuntimeAuthUpdate(update) {
		return
	}
	if s.authUpdates != nil {
		select {
		case s.authUpdates <- update:
			return
		default:
		}
	}
	s.plusCredentialMu.Lock()
	stopped := s.plusCredentialStopped
	s.plusCredentialMu.Unlock()
	if stopped {
		return
	}
	s.handleAuthUpdate(context.Background(), update)
}

// dispatchPlusCredentialUpdate hands an already-fenced update to the ordered
// runtime-auth pipelines. When the service queue is saturated it blocks
// instead of applying inline so an inline delete cannot overtake an earlier
// queued add for the same ID.
func (s *Service) dispatchPlusCredentialUpdate(ctx context.Context, update watcher.AuthUpdate) {
	if watcherInstance := s.currentWatcher(); watcherInstance != nil && watcherInstance.DispatchRuntimeAuthUpdate(update) {
		return
	}
	if s.authUpdates != nil {
		select {
		case s.authUpdates <- update:
			return
		case <-ctx.Done():
			return
		}
	}
	s.handleAuthUpdate(ctx, update)
}

// dropSupersededPlusCredentialUpdate drops a config-credential add whose ID is
// no longer part of the active desired set. This is the consumer-side fence
// for the case where a queued add is delivered after a newer activation.
func (s *Service) dropSupersededPlusCredentialUpdate(update watcher.AuthUpdate) bool {
	if s == nil {
		return false
	}
	isAdd := update.Action == watcher.AuthUpdateActionAdd || update.Action == watcher.AuthUpdateActionModify
	isDelete := update.Action == watcher.AuthUpdateActionDelete
	if !isAdd && !isDelete {
		return false
	}
	id := authUpdateID(update)
	if !isPlusConfigCredentialID(id) {
		return false
	}
	s.plusCredentialMu.Lock()
	defer s.plusCredentialMu.Unlock()
	if s.plusCredentialIDs == nil {
		return false
	}
	if s.plusCredentialStopped {
		return true
	}
	_, desired := s.plusCredentialIDs[id]
	if isDelete {
		// A delete for a credential that is desired again was superseded by a
		// newer activation (rapid remove -> re-add); applying it would remove
		// the fresh credential.
		if desired {
			log.Debugf("dropping superseded plus credential delete id=%s", id)
		}
		return desired
	}
	if !desired {
		log.Debugf("dropping superseded plus credential update id=%s", id)
	}
	return !desired
}

// isPlusConfigCredentialID reports whether the auth ID belongs to a config
// driven Plus credential managed by the activator.
func isPlusConfigCredentialID(id string) bool {
	return strings.HasPrefix(id, "kiro:config:") || strings.HasPrefix(id, "bt:config:")
}

// kiroCredentialFingerprints returns the config and token fingerprints stamped
// by the synthesizer, if present.
func kiroCredentialFingerprints(auth *coreauth.Auth) (string, string) {
	if auth == nil || auth.Attributes == nil {
		return "", ""
	}
	return strings.TrimSpace(auth.Attributes[synthesizer.KiroConfigFingerprintAttr]),
		strings.TrimSpace(auth.Attributes[synthesizer.KiroTokenFingerprintAttr])
}

// preserveKiroRuntimeState keeps status/cooldown fields owned by the manager
// when a credential is republished.
func preserveKiroRuntimeState(existing, merged *coreauth.Auth) {
	if existing == nil || merged == nil {
		return
	}
	merged.Status = existing.Status
	merged.StatusMessage = existing.StatusMessage
	merged.Unavailable = existing.Unavailable
	merged.NextRetryAfter = existing.NextRetryAfter
	merged.NextRefreshAfter = existing.NextRefreshAfter
	merged.Quota = existing.Quota
	merged.CreatedAt = existing.CreatedAt
}

// preserveKiroTokenMetadata copies the runtime token material over a config
// snapshot so a presentation-only reload cannot roll back a refresh rotation.
func preserveKiroTokenMetadata(existing, merged *coreauth.Auth) {
	if existing == nil || merged == nil || merged.Metadata == nil || existing.Metadata == nil {
		return
	}
	for _, key := range []string{"access_token", "refresh_token", "expires_at"} {
		if value, ok := existing.Metadata[key]; ok {
			merged.Metadata[key] = value
		}
	}
}

// reconcileKiroConfigAuth decides whether a synthesized config credential must
// be republished. Fingerprints separate presentation/id inputs from token
// material: an unchanged config keeps the runtime tokens, a changed token file
// replaces them.
func reconcileKiroConfigAuth(existing, desired *coreauth.Auth) (*coreauth.Auth, bool, bool) {
	if desired == nil {
		return nil, false, false
	}
	if existing == nil {
		candidate := desired.Clone()
		return candidate, true, kiroAuthAccessToken(candidate) == ""
	}
	if kiroConfigAuthEquivalent(existing, desired) {
		return nil, false, kiroAuthAccessToken(existing) == ""
	}
	_, existingToken := kiroCredentialFingerprints(existing)
	_, desiredToken := kiroCredentialFingerprints(desired)
	merged := desired.Clone()
	preserveKiroRuntimeState(existing, merged)
	// Matching token fingerprints mean the configured secret material did not
	// change, so a presentation-only reload must keep the runtime tokens.
	if desiredToken != "" && existingToken != "" && existingToken == desiredToken {
		preserveKiroTokenMetadata(existing, merged)
	}
	return merged, true, kiroAuthAccessToken(merged) == ""
}

func (s *Service) btLoginBackoffActive(id string, now time.Time) bool {
	s.plusCredentialMu.Lock()
	defer s.plusCredentialMu.Unlock()
	failure, ok := s.btLoginFailures[id]
	return ok && now.Before(failure.nextAttempt)
}

func (s *Service) recordBTLoginFailure(id string) {
	s.plusCredentialMu.Lock()
	defer s.plusCredentialMu.Unlock()
	if s.btLoginFailures == nil {
		s.btLoginFailures = make(map[string]btLoginFailure)
	}
	failure := s.btLoginFailures[id]
	failure.attempts++
	shift := failure.attempts - 1
	if shift > btLoginBackoffShiftCap {
		shift = btLoginBackoffShiftCap
	}
	delay := btLoginBackoffBase << uint(shift)
	if delay > btLoginBackoffMax {
		delay = btLoginBackoffMax
	}
	failure.nextAttempt = time.Now().Add(delay)
	s.btLoginFailures[id] = failure
}

func (s *Service) clearBTLoginFailure(id string) {
	s.plusCredentialMu.Lock()
	defer s.plusCredentialMu.Unlock()
	delete(s.btLoginFailures, id)
}

func kiroConfigAuthEquivalent(existing, desired *coreauth.Auth) bool {
	if existing == nil || desired == nil {
		return false
	}
	if existing.ProxyURL != desired.ProxyURL || existing.Prefix != desired.Prefix {
		return false
	}
	if !stringMapEqual(existing.Attributes, desired.Attributes) {
		return false
	}
	for _, key := range []string{
		"type", "access_token", "refresh_token", "profile_arn", "expires_at",
		"auth_method", "provider", "client_id", "client_secret", "client_id_hash",
		"email", "start_url", "region", "preferred_endpoint",
	} {
		if !metaValueEqual(existing.Metadata[key], desired.Metadata[key]) {
			return false
		}
	}
	return true
}

func kiroAuthAccessToken(auth *coreauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	return strings.TrimSpace(metaString(auth.Metadata["access_token"]))
}

func metaString(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}

func metaValueEqual(left, right any) bool {
	return metaString(left) == metaString(right)
}

func stringMapEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func kiroTokenFilePaths(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(cfg.KiroKey))
	paths := make([]string, 0, len(cfg.KiroKey))
	for i := range cfg.KiroKey {
		raw := strings.TrimSpace(cfg.KiroKey[i].TokenFile)
		if raw == "" {
			continue
		}
		path := expandPlusTokenPath(raw)
		if abs, errAbs := filepath.Abs(path); errAbs == nil {
			path = abs
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	return paths
}

func expandPlusTokenPath(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		home, errHome := os.UserHomeDir()
		if errHome != nil {
			return path
		}
		rest := strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/")
		rest = strings.TrimPrefix(rest, `\`)
		if rest == "" {
			return home
		}
		return filepath.Join(home, rest)
	}
	return path
}

func kiroTokenPathMatches(paths []string, name string) bool {
	candidate, errAbs := filepath.Abs(name)
	if errAbs != nil {
		candidate = name
	}
	for _, path := range paths {
		if strings.EqualFold(filepath.Clean(path), filepath.Clean(candidate)) {
			return true
		}
	}
	return false
}

// newKiroTokenWatcher subscribes to every distinct parent directory of the
// configured token files. It returns nil when there is nothing to watch or no
// directory could be watched.
func (s *Service) newKiroTokenWatcher(paths []string) *fsnotify.Watcher {
	if len(paths) == 0 {
		return nil
	}
	fileWatcher, errNew := fsnotify.NewWatcher()
	if errNew != nil {
		log.Warnf("kiro token file watch: %v", errNew)
		return nil
	}
	dirs := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		dir := filepath.Dir(path)
		if _, ok := dirs[dir]; ok {
			continue
		}
		if errAdd := fileWatcher.Add(dir); errAdd != nil {
			log.Warnf("kiro token file watch %s: %v", dir, errAdd)
			continue
		}
		dirs[dir] = struct{}{}
	}
	if len(dirs) == 0 {
		if errClose := fileWatcher.Close(); errClose != nil {
			log.Debugf("kiro token file watch close: %v", errClose)
		}
		return nil
	}
	return fileWatcher
}

func (s *Service) watchKiroTokenFiles(ctx context.Context, generation uint64, paths []string, fileWatcher *fsnotify.Watcher) {
	if fileWatcher == nil {
		return
	}
	defer func() {
		if errClose := fileWatcher.Close(); errClose != nil {
			log.Debugf("kiro token file watch close: %v", errClose)
		}
	}()

	var debounce *time.Timer
	var debounceC <-chan time.Time
	stopDebounce := func() {
		if debounce != nil {
			debounce.Stop()
			debounce = nil
			debounceC = nil
		}
	}
	defer stopDebounce()

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-fileWatcher.Events:
			if !ok {
				return
			}
			if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) == 0 {
				continue
			}
			if !kiroTokenPathMatches(paths, event.Name) {
				continue
			}
			stopDebounce()
			debounce = time.NewTimer(250 * time.Millisecond)
			debounceC = debounce.C
		case errEvent, ok := <-fileWatcher.Errors:
			if !ok {
				return
			}
			log.Debugf("kiro token file watch: %v", errEvent)
		case <-debounceC:
			debounceC = nil
			if !s.plusCredentialGenerationCurrent(generation) {
				return
			}
			nextCfg := s.currentConfig()
			if nextCfg == nil {
				return
			}
			log.Info("kiro token file changed; re-applying config credentials")
			s.startPlusCredentialActivation(ctx, nextCfg)
		}
	}
}
