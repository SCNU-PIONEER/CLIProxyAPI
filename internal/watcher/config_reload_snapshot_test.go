package watcher

import (
	"bytes"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestReloadConfigHashesSecretAndCleansConflictingLegacyFields(t *testing.T) {
	configPath, authDir := newIssue6631ConfigFile(t)
	raw := "port: 8317\nauth-dir: " + authDir + "\ndebug: true\nobservability:\n  logs:\n    debug: false\nremote-management:\n  secret-key: \"plaintext-secret\"\n"
	if errWrite := os.WriteFile(configPath, []byte(raw), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}

	w := &Watcher{
		configPath:     configPath,
		authDir:        authDir,
		reloadCallback: func(*config.Config) {},
	}
	w.ReloadConfigIfChanged()

	data, errRead := os.ReadFile(configPath)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if strings.Contains(string(data), "plaintext-secret") {
		t.Fatalf("reload left the plaintext secret on disk: %s", string(data))
	}
	if !strings.Contains(string(data), "$2") {
		t.Fatalf("reload did not persist the hashed secret: %s", string(data))
	}
	if strings.Contains(string(data), "\ndebug:") {
		t.Fatalf("reload did not remove the conflicting legacy debug field: %s", string(data))
	}
	w.clientsMutex.RLock()
	applied := w.config
	w.clientsMutex.RUnlock()
	if applied == nil {
		t.Fatal("config not applied")
	}
	if applied.RemoteManagement.SecretKey == "" || applied.RemoteManagement.SecretKey == "plaintext-secret" {
		t.Fatalf("applied secret = %q, want a bcrypt hash", applied.RemoteManagement.SecretKey)
	}
}

func TestReloadConfigIfChangedRejectsWhitespaceOnlyWrite(t *testing.T) {
	configPath, authDir := newIssue6631ConfigFile(t)
	if errWrite := writeIssue6631Config(configPath, authDir, 1); errWrite != nil {
		t.Fatal(errWrite)
	}

	var applied []int
	w := &Watcher{
		configPath:     configPath,
		authDir:        authDir,
		reloadCallback: func(cfg *config.Config) { applied = append(applied, cfg.RequestRetry) },
	}
	w.ReloadConfigIfChanged()
	if want := []int{1}; !reflect.DeepEqual(applied, want) {
		t.Fatalf("applied = %v, want %v", applied, want)
	}
	w.clientsMutex.RLock()
	hashBefore := w.lastConfigHash
	w.clientsMutex.RUnlock()

	// A truncated in-place write can expose only whitespace. Reload must ignore
	// it instead of parsing it as a valid empty config with defaults.
	if errWrite := os.WriteFile(configPath, []byte("  \n\t\n"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	w.ReloadConfigIfChanged()

	if want := []int{1}; !reflect.DeepEqual(applied, want) {
		t.Fatalf("whitespace-only config was applied: %v", applied)
	}
	w.clientsMutex.RLock()
	hashAfter := w.lastConfigHash
	w.clientsMutex.RUnlock()
	if hashAfter != hashBefore {
		t.Fatalf("lastConfigHash changed from %q to %q on an ignored write", hashBefore, hashAfter)
	}
}

func TestReloadConfigIfChangedDoesNotObserveInPlaceWriteWindow(t *testing.T) {
	configPath, authDir := newIssue6631ConfigFile(t)
	if errWrite := writeIssue6631Config(configPath, authDir, 1); errWrite != nil {
		t.Fatal(errWrite)
	}

	var applied []int
	w := &Watcher{
		configPath:     configPath,
		authDir:        authDir,
		reloadCallback: func(cfg *config.Config) { applied = append(applied, cfg.RequestRetry) },
	}
	w.ReloadConfigIfChanged()
	if want := []int{1}; !reflect.DeepEqual(applied, want) {
		t.Fatalf("applied = %v, want %v", applied, want)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- config.UpdateConfigFile(configPath, func(data []byte) ([]byte, error) {
			close(entered)
			<-release
			return bytes.Replace(data, []byte("request-retry: 1"), []byte("request-retry: 2"), 1), nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("in-place write did not start")
	}

	reloadDone := make(chan struct{})
	go func() {
		w.ReloadConfigIfChanged()
		close(reloadDone)
	}()
	// The bounded wait only gives an unserialized reload the chance to finish;
	// a reload that reads the file under the shared lock stays blocked.
	select {
	case <-reloadDone:
		t.Fatal("reload completed while the in-place write was still running")
	case <-time.After(200 * time.Millisecond):
	}
	w.clientsMutex.RLock()
	requestRetry := 0
	if w.config != nil {
		requestRetry = w.config.RequestRetry
	}
	w.clientsMutex.RUnlock()
	if requestRetry != 1 {
		t.Fatalf("reload applied request-retry %d during the write window, want 1", requestRetry)
	}

	close(release)
	if errWrite := <-writeDone; errWrite != nil {
		t.Fatal(errWrite)
	}
	select {
	case <-reloadDone:
	case <-time.After(5 * time.Second):
		t.Fatal("reload did not resume after the in-place write completed")
	}
	if want := []int{1, 2}; !reflect.DeepEqual(applied, want) {
		t.Fatalf("applied = %v, want %v", applied, want)
	}
	w.clientsMutex.RLock()
	reloaded := w.config
	w.clientsMutex.RUnlock()
	if reloaded == nil {
		t.Fatal("config not applied")
	}
	if reloaded.AuthDir == "" || reloaded.AuthDir != authDir {
		t.Fatalf("auth-dir = %q, want the configured %q", reloaded.AuthDir, authDir)
	}
	if !reloaded.WebsocketAuth {
		t.Fatal("websocket-auth default leaked in, the write window was observed")
	}
}
