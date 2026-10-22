package api

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/managementasset"
)

func TestEmbeddedPlusExportServesRoutesAndAssets(t *testing.T) {
	if !managementasset.HasEmbeddedAssets() {
		t.Skip("optional Plus export not built; CI runs npm run build:embed before this test")
	}
	s := newTestServer(t)
	for _, route := range []string{"/plus/", "/plus/login/", "/plus/oauth/", "/plus/config/"} {
		rr := httptest.NewRecorder()
		s.engine.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, route, nil))
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "/plus/_next/static/") {
			t.Fatalf("exported route %s: status %d", route, rr.Code)
		}
		asset := regexp.MustCompile(`(?:src|href)="(/plus/_next/static/[^\"]+)"`).FindStringSubmatch(rr.Body.String())
		if len(asset) != 2 {
			t.Fatalf("no exported static asset found in %s", route)
		}
		assetResponse := httptest.NewRecorder()
		s.engine.ServeHTTP(assetResponse, httptest.NewRequest(http.MethodGet, asset[1], nil))
		if assetResponse.Code != http.StatusOK || assetResponse.Body.Len() == 0 || assetResponse.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("asset %s: status %d", asset[1], assetResponse.Code)
		}
	}
}
