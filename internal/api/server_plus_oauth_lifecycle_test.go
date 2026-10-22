package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type lifecyclePlusAuthenticator struct {
	started   chan struct{}
	cancelled chan struct{}
}

func (*lifecyclePlusAuthenticator) Provider() string            { return "kilo" }
func (*lifecyclePlusAuthenticator) RefreshLead() *time.Duration { return nil }
func (a *lifecyclePlusAuthenticator) Login(ctx context.Context, _ *config.Config, _ *sdkAuth.LoginOptions) (*coreauth.Auth, error) {
	close(a.started)
	<-ctx.Done()
	close(a.cancelled)
	return nil, ctx.Err()
}

func TestPlusOAuthCancelledByServerLifecycle(t *testing.T) {
	for _, action := range []string{"stop", "home", "disable-management"} {
		t.Run(action, func(t *testing.T) {
			s := newTestServer(t)
			t.Cleanup(s.mgmt.ClosePlusOAuthSessions)
			a := &lifecyclePlusAuthenticator{started: make(chan struct{}), cancelled: make(chan struct{})}
			s.mgmt.SetSDKAuthManager(sdkAuth.NewManager(nil, a))
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodGet, "/v8/management/oauth/auth-url?provider=kilo", nil)
			if !s.mgmt.StartPlusOAuthV8(c) {
				t.Fatal("Plus start not handled")
			}
			select {
			case <-a.started:
			case <-time.After(3 * time.Second):
				t.Fatal("login not started")
			}
			switch action {
			case "stop":
				if err := s.Stop(t.Context()); err != nil {
					t.Fatal(err)
				}
			case "home":
				cfg := s.getConfig().CloneForRuntime()
				cfg.Home.Enabled = true
				s.UpdateClients(cfg)
			case "disable-management":
				cfg := s.getConfig().CloneForRuntime()
				cfg.RemoteManagement.SecretKey = ""
				s.envManagementSecret = false
				s.UpdateClients(cfg)
			}
			select {
			case <-a.cancelled:
			case <-time.After(3 * time.Second):
				t.Fatal("pending login survived lifecycle shutdown")
			}
		})
	}
}
