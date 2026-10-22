package api

import (
	"io/fs"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/managementasset"
)

// servePlusControlPanel keeps the optional bundled Plus UI separate from the
// upstream control panel. Both honor the same Home and panel-disable boundary.
func (s *Server) servePlusControlPanel(c *gin.Context) {
	s.servePlusAsset(c, managementasset.GetEmbeddedFS())
}

func (s *Server) servePlusAsset(c *gin.Context, assets fs.FS) {
	cfg := s.getConfig()
	if cfg == nil || cfg.Home.Enabled || cfg.RemoteManagement.DisableControlPanel || assets == nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	if _, err := fs.Stat(assets, "index.html"); err != nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	assetPath := strings.TrimPrefix(c.Request.URL.Path, "/plus")
	if strings.ContainsAny(assetPath, "\\\x00") {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	for _, segment := range strings.Split(assetPath, "/") {
		if segment == "." || segment == ".." {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
	}
	request := c.Request.Clone(c.Request.Context())
	request.URL.Path = assetPath
	request.URL.RawPath = ""
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Cache-Control", "no-store")
	if strings.HasPrefix(assetPath, "/_next/static/") {
		c.Header("Cache-Control", "public, max-age=31536000, immutable")
	}
	http.FileServer(http.FS(assets)).ServeHTTP(c.Writer, request)
}
