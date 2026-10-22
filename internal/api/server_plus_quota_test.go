package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPlusKiroQuotaRequiresManagementAuth(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "quota-management-key")
	server := newTestServer(t)
	for _, authorized := range []bool{false, true} {
		req := httptest.NewRequest(http.MethodGet, "/v8/management/observability/quota/kiro", nil)
		want := http.StatusUnauthorized
		if authorized {
			req.Header.Set("Authorization", "Bearer quota-management-key")
			want = http.StatusBadRequest // No configured Kiro credential; never contact a provider.
		}
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)
		if rr.Code != want {
			t.Fatalf("authorized=%v: got %d, want %d: %s", authorized, rr.Code, want, rr.Body.String())
		}
	}
}
