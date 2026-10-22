package bt

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
)

// stubBTLoginServer serves a successful login payload so LoginContext can be
// exercised without contacting the real BaoTa API.
func stubBTLoginServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The client hex-decodes data and then URL-unescapes it before JSON decoding.
		body := `{"data":"` + hex.EncodeToString([]byte(`{"uid":"42","access_key":"ak"}`)) + `"}`
		_, _ = w.Write([]byte(body))
	}))
	previous := loginURL
	loginURL = server.URL
	t.Cleanup(func() {
		loginURL = previous
		server.Close()
	})
	return server
}

func TestLoginContextHonorsCancellation(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer server.Close()
	previous := loginURL
	loginURL = server.URL
	defer func() { loginURL = previous }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := LoginContext(ctx, "13800000000", "cGFzcw==", "")
	if err == nil {
		t.Fatal("LoginContext succeeded with a cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	close(release)
}

func TestLoginContextUsesProxy(t *testing.T) {
	loginServer := stubBTLoginServer(t)
	loginTarget, errParse := url.Parse(loginServer.URL)
	if errParse != nil {
		t.Fatalf("parse login server url: %v", errParse)
	}

	var mu sync.Mutex
	var connects []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		connects = append(connects, r.Host)
		mu.Unlock()
		if r.Method == http.MethodConnect {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		http.Error(w, "proxy only", http.StatusBadGateway)
	}))
	defer proxy.Close()

	if _, err := LoginContext(context.Background(), "13800000000", "cGFzcw==", proxy.URL); err == nil {
		t.Fatal("LoginContext succeeded through a rejecting proxy")
	}
	mu.Lock()
	seen := append([]string(nil), connects...)
	mu.Unlock()
	if len(seen) == 0 || seen[0] != loginTarget.Host {
		t.Fatalf("proxy saw requests %v, want %s", seen, loginTarget.Host)
	}
}

func TestLoginContextSuccessWithoutProxy(t *testing.T) {
	stubBTLoginServer(t)
	token, err := LoginContext(context.Background(), "13800000000", "cGFzcw==", "")
	if err != nil {
		t.Fatalf("LoginContext() error = %v", err)
	}
	if token.UID != "42" || token.AccessKey != "ak" {
		t.Fatalf("token = %+v", token)
	}
	if token.Type != "bt" {
		t.Fatalf("token type = %q", token.Type)
	}
}
