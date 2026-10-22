package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
)

func TestPlusUsageFollowsConfiguration(t *testing.T) {
	previous := usage.StatisticsEnabled()
	t.Cleanup(func() { usage.SetStatisticsEnabled(previous) })
	usage.SetStatisticsEnabled(true)
	server := newTestServer(t)
	if usage.StatisticsEnabled() {
		t.Fatal("usage aggregation enabled despite disabled startup configuration")
	}
	cfg := server.cfg.CloneForRuntime()
	cfg.UsageStatisticsEnabled = true
	server.UpdateClients(cfg)
	if !usage.StatisticsEnabled() {
		t.Fatal("usage aggregation not enabled after configuration reload")
	}
	cfg = cfg.CloneForRuntime()
	cfg.UsageStatisticsEnabled = false
	server.UpdateClients(cfg)
	if usage.StatisticsEnabled() {
		t.Fatal("usage aggregation not disabled after configuration reload")
	}
}

func TestPlusUsageV8RoutesRequireManagementAuth(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "plus-management-key")
	server := newTestServer(t)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/v8/management/observability/usage", ""},
		{http.MethodGet, "/v8/management/observability/usage/export", ""},
		{http.MethodPost, "/v8/management/observability/usage/import", `{"version":1,"usage":{}}`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			for _, authorized := range []bool{false, true} {
				req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
				req.Header.Set("Content-Type", "application/json")
				want := http.StatusUnauthorized
				if authorized {
					req.Header.Set("Authorization", "Bearer plus-management-key")
					want = http.StatusOK
				}
				rr := httptest.NewRecorder()
				server.engine.ServeHTTP(rr, req)
				if rr.Code != want {
					t.Fatalf("authorized=%v: status %d, want %d: %s", authorized, rr.Code, want, rr.Body.String())
				}
			}
		})
	}
}
