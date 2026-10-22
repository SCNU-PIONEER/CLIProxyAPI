package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/gin-gonic/gin"
)

func TestPlusAssetsRespectControlPanelBoundary(t *testing.T) {
	s := newTestServer(t)
	assets := fstest.MapFS{
		"index.html":          {Data: []byte("plus dashboard")},
		"settings/index.html": {Data: []byte("settings page")},
		"_next/static/app.js": {Data: []byte("console.log('plus')")},
	}
	router := gin.New()
	router.GET("/plus/*filepath", func(c *gin.Context) { s.servePlusAsset(c, assets) })
	router.HEAD("/plus/*filepath", func(c *gin.Context) { s.servePlusAsset(c, assets) })
	for _, tc := range []struct {
		path   string
		status int
		body   string
	}{
		{"/plus/", 200, "plus dashboard"},
		{"/plus/settings/", 200, "settings page"},
		{"/plus/_next/static/missing.js", 404, ""},
		{"/plus/../config.yaml", 404, ""},
	} {
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rr.Code != tc.status || (tc.body != "" && rr.Body.String() != tc.body) {
			t.Fatalf("%s: status=%d body=%s", tc.path, rr.Code, rr.Body.String())
		}
	}
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodHead, "/plus/", nil))
	if rr.Code != 200 || rr.Body.Len() != 0 {
		t.Fatalf("HEAD: %d %s", rr.Code, rr.Body.String())
	}
	for _, home := range []bool{false, true} {
		s.cfg.Home.Enabled = home
		s.cfg.RemoteManagement.DisableControlPanel = !home
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/plus/", nil))
		if rr.Code != 404 {
			t.Fatalf("disabled/home=%v: got %d", home, rr.Code)
		}
	}
}
