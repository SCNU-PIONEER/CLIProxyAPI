package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type qoderWireCapture struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (c *qoderWireCapture) handler(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies = append(c.bodies, body)
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"message":"fixture stop"}`))
	}
}

func (c *qoderWireCapture) snapshot() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.bodies...)
}

func newQoderTestExecutor(cfg *config.Config, endpoint string) *QoderExecutor {
	exec := NewQoderExecutor(cfg)
	exec.endpointOverride = endpoint
	return exec
}

func newQoderTestAuth() *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       "qoder-test",
		Provider: "qoder",
		Metadata: map[string]any{
			"access_token": "test-token",
			"uid":          "user-1",
			"name":         "Tester",
			"email":        "tester@example.com",
			"machine_id":   "machine-1",
		},
	}
}

func qoderOpenAIPayload() []byte {
	return []byte(`{"model":"qwen3-coder-plus","max_tokens":1024,"messages":[{"role":"user","content":"hello"}]}`)
}

func qoderPayloadRuleConfig() *config.Config {
	return &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
		Models: []config.PayloadModelRule{{Name: "*", Protocol: qoderProtocol}},
		Params: map[string]any{
			"parameters.max_tokens": 123,
			"messages.0.content":    "payload-content",
		},
	}}}}
}

func runQoderExecute(t *testing.T, exec *QoderExecutor) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10e9)
	defer cancel()
	_, err := exec.Execute(ctx, newQoderTestAuth(), cliproxyexecutor.Request{
		Model:   "qwen3-coder-plus",
		Payload: qoderOpenAIPayload(),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")})
	return err
}

// TestQoderPayloadGateTargetsFinalNativeJSON proves config payload rules apply
// to the final native Qoder envelope, not to the intermediate OpenAI body.
func TestQoderPayloadGateTargetsFinalNativeJSON(t *testing.T) {
	capture := &qoderWireCapture{}
	server := httptest.NewServer(capture.handler(http.StatusBadRequest))
	defer server.Close()

	exec := newQoderTestExecutor(qoderPayloadRuleConfig(), server.URL)
	if err := runQoderExecute(t, exec); err == nil {
		t.Fatalf("expected upstream status error from fixture")
	}

	bodies := capture.snapshot()
	if len(bodies) != 1 {
		t.Fatalf("expected exactly one wire attempt, got %d", len(bodies))
	}
	wire := bodies[0]
	if got := gjson.GetBytes(wire, "parameters.max_tokens").Int(); got != 123 {
		t.Fatalf("wire parameters.max_tokens = %d, want 123 (gate did not target final Qoder JSON): %s", got, wire)
	}
	if got := gjson.GetBytes(wire, "messages.0.content").String(); got != "payload-content" {
		t.Fatalf("wire messages.0.content = %q, want payload-content", got)
	}
	if gjson.GetBytes(wire, "max_tokens").Exists() {
		t.Fatalf("source-shaped field leaked into the Qoder envelope: %s", wire)
	}
}

// TestQoderStreamPayloadGateTargetsFinalNativeJSON covers the streaming path.
func TestQoderStreamPayloadGateTargetsFinalNativeJSON(t *testing.T) {
	capture := &qoderWireCapture{}
	server := httptest.NewServer(capture.handler(http.StatusBadRequest))
	defer server.Close()

	exec := newQoderTestExecutor(qoderPayloadRuleConfig(), server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10e9)
	defer cancel()
	_, err := exec.ExecuteStream(ctx, newQoderTestAuth(), cliproxyexecutor.Request{
		Model:   "qwen3-coder-plus",
		Payload: qoderOpenAIPayload(),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")})
	if err == nil {
		t.Fatalf("expected upstream status error from fixture")
	}

	bodies := capture.snapshot()
	if len(bodies) != 1 {
		t.Fatalf("expected exactly one wire attempt, got %d", len(bodies))
	}
	wire := bodies[0]
	if got := gjson.GetBytes(wire, "parameters.max_tokens").Int(); got != 123 {
		t.Fatalf("wire parameters.max_tokens = %d, want 123 (gate did not target final Qoder JSON): %s", got, wire)
	}
	if got := gjson.GetBytes(wire, "messages.0.content").String(); got != "payload-content" {
		t.Fatalf("wire messages.0.content = %q, want payload-content", got)
	}
}
