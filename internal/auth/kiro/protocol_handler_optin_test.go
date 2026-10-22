package kiro

import (
	"errors"
	"testing"
)

// stubProtocolHandlerInstall replaces the OS-level install hooks for a test.
func stubProtocolHandlerInstall(t *testing.T, installed bool) *struct {
	called bool
	port   int
} {
	t.Helper()
	record := &struct {
		called bool
		port   int
	}{}
	oldInstall := installProtocolHandlerFunc
	oldInstalled := isProtocolHandlerInstalledFunc
	t.Cleanup(func() {
		installProtocolHandlerFunc = oldInstall
		isProtocolHandlerInstalledFunc = oldInstalled
	})
	installProtocolHandlerFunc = func(port int) error {
		record.called = true
		record.port = port
		return nil
	}
	isProtocolHandlerInstalledFunc = func() bool { return installed }
	return record
}

// TestSetupProtocolHandlerRequiresOptIn ensures the OS-level protocol handler
// is never installed without an explicit user request.
func TestSetupProtocolHandlerRequiresOptIn(t *testing.T) {
	record := stubProtocolHandlerInstall(t, false)

	err := SetupProtocolHandlerIfNeeded(19876, false)
	if !errors.Is(err, ErrProtocolHandlerOptInRequired) {
		t.Fatalf("SetupProtocolHandlerIfNeeded(false) error = %v, want ErrProtocolHandlerOptInRequired", err)
	}
	if record.called {
		t.Fatal("protocol handler was installed without opt-in")
	}
}

// TestSetupProtocolHandlerInstallsWhenOptedIn ensures the explicit opt-in path
// still installs the handler.
func TestSetupProtocolHandlerInstallsWhenOptedIn(t *testing.T) {
	record := stubProtocolHandlerInstall(t, false)

	if err := SetupProtocolHandlerIfNeeded(4321, true); err != nil {
		t.Fatalf("SetupProtocolHandlerIfNeeded(true): %v", err)
	}
	if !record.called {
		t.Fatal("protocol handler install was not attempted after opt-in")
	}
	if record.port != 4321 {
		t.Fatalf("installed handler port = %d, want 4321", record.port)
	}
}

// TestSetupProtocolHandlerSkipsExistingHandler ensures an already registered
// handler is not rewritten.
func TestSetupProtocolHandlerSkipsExistingHandler(t *testing.T) {
	record := stubProtocolHandlerInstall(t, true)

	if err := SetupProtocolHandlerIfNeeded(19876, true); err != nil {
		t.Fatalf("SetupProtocolHandlerIfNeeded(existing): %v", err)
	}
	if record.called {
		t.Fatal("existing protocol handler was reinstalled")
	}
}
