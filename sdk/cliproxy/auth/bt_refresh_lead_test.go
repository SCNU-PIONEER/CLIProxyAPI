package auth

import (
	"context"
	"testing"
	"time"
)

// TestAutoRefreshCallsExecutorForBTWithoutExpiry proves the scheduler path that
// keeps config-managed BT credentials alive: a BT auth carries no expiry, so it
// is only refreshed once a refresh lead is registered for the provider.
func TestAutoRefreshSchedulesConfigBTByInterval(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	exec := &mockOAuthErrorExecutor{id: "bt"}
	manager.RegisterExecutor(exec)

	// Config activation stamps this per-auth cadence (6h). It is a periodic
	// interval, not a claimed token expiry.
	auth := &Auth{
		ID:              "bt-config-1",
		Provider:        "bt",
		Status:          StatusActive,
		LastRefreshedAt: time.Now().Add(-7 * time.Hour),
		Attributes:      map[string]string{"bt_phone": "13800000001"},
		Metadata: map[string]any{
			"type":                     "bt",
			"uid":                      "uid-1",
			"access_key":               "key-1",
			"refresh_interval_seconds": int64(21600),
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.StartAutoRefresh(ctx, 10*time.Millisecond)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if exec.refreshCalls.Load() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	manager.StopAutoRefresh()
	if got := exec.refreshCalls.Load(); got == 0 {
		t.Fatal("auto refresh never called the config BT executor via refresh_interval_seconds")
	}
}

func TestAutoRefreshLeavesFileBackedBTUnscheduled(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	exec := &mockOAuthErrorExecutor{id: "bt"}
	manager.RegisterExecutor(exec)

	// File-backed BT auths have no config entry to re-login from, so they must
	// not be scheduled by a global policy.
	auth := &Auth{
		ID:              "bt-file-1",
		Provider:        "bt",
		Status:          StatusActive,
		LastRefreshedAt: time.Now().Add(-7 * time.Hour),
		Metadata:        map[string]any{"type": "bt", "uid": "uid-1", "access_key": "key-1"},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.StartAutoRefresh(ctx, 10*time.Millisecond)
	time.Sleep(300 * time.Millisecond)
	manager.StopAutoRefresh()
	if got := exec.refreshCalls.Load(); got != 0 {
		t.Fatalf("file-backed BT auth was refreshed %d times without a per-auth schedule", got)
	}
}
