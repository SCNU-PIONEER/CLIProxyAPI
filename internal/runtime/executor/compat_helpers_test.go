package executor

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/httptransport"
)

// TestNewProxyAwareHTTPClientDecoratesExactlyOnce guards the compat wrapper
// against double decoration: helps.NewProxyAwareHTTPClient already applies the
// context decorator, so the wrapper must return that client unchanged. A second
// Decorate call here would wrap every transport twice and duplicate transport
// observation for providers that use this helper.
func TestNewProxyAwareHTTPClientDecoratesExactlyOnce(t *testing.T) {
	var decoratorCalls int32
	ctx := httptransport.WithDecorator(context.Background(), func(next http.RoundTripper) http.RoundTripper {
		atomic.AddInt32(&decoratorCalls, 1)
		return next
	})

	client := newProxyAwareHTTPClient(ctx, nil, nil, 0)
	if client == nil {
		t.Fatal("newProxyAwareHTTPClient returned nil")
	}
	if got := atomic.LoadInt32(&decoratorCalls); got != 1 {
		t.Fatalf("decorator applied %d times, want exactly 1", got)
	}
}
