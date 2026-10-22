package tui

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPlusUsageUsesV8Management(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v8/management/observability/usage" || r.Header.Get("Authorization") != "Bearer test-key" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"total_requests":7}}`))
	}))
	defer server.Close()
	result, err := NewClientWithBaseURL(server.URL, "test-key").GetUsage()
	if err != nil || result["usage"] == nil {
		t.Fatalf("v8 usage request failed: %v, %v", result, err)
	}
}
