package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/translator/kiro/common"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

var kiroTestTokenSeq int64

type kiroWireCapture struct {
	mu      sync.Mutex
	payload [][]byte
	origins []string
}

func (c *kiroWireCapture) handler(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		origin := gjson.GetBytes(body, "conversationState.currentMessage.userInputMessage.origin").String()
		c.mu.Lock()
		c.payload = append(c.payload, body)
		c.origins = append(c.origins, origin)
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"message":"fixture stop"}`))
	}
}

func (c *kiroWireCapture) snapshot() ([][]byte, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.payload...), append([]string(nil), c.origins...)
}

func newKiroTestAuth(token string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       token,
		Provider: "kiro",
		Metadata: map[string]any{
			"access_token": token,
			"profile_arn":  "arn:aws:codewhisperer:us-east-1:111111111111:profile/TEST",
			"region":       "us-east-1",
		},
	}
}

func newKiroTestExecutor(cfg *config.Config, endpoints []kiroEndpointConfig) *KiroExecutor {
	exec := NewKiroExecutor(cfg)
	exec.endpointOverride = endpoints
	return exec
}

// stubKiroHumanLikeDelay removes the anti-bot jitter so wire assertions stay fast.
func stubKiroHumanLikeDelay(t *testing.T) {
	t.Helper()
	original := kiroHumanLikeDelay
	kiroHumanLikeDelay = func() {}
	t.Cleanup(func() { kiroHumanLikeDelay = original })
}

func kiroClaudePayloadWithThinking(budget int64) []byte {
	return []byte(`{"model":"claude-sonnet-4-5","max_tokens":1024,"system":"base system","messages":[{"role":"user","content":"hello"}],"thinking":{"type":"enabled","budget_tokens":` + strconv.FormatInt(budget, 10) + `}}`)
}

func runKiroExecute(t *testing.T, exec *KiroExecutor, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := exec.Execute(ctx, newKiroTestAuth(fmt.Sprintf("test-token-%s-%d", t.Name(), atomic.AddInt64(&kiroTestTokenSeq, 1))), req, opts)
	return err
}

// TestKiroFinalPayloadGateTargetsWireJSON proves the payload gate applies to the
// final Kiro JSON (after the builder), not to the source-shaped request body.
func TestKiroFinalPayloadGateTargetsWireJSON(t *testing.T) {
	stubKiroHumanLikeDelay(t)
	capture := &kiroWireCapture{}
	server := httptest.NewServer(capture.handler(http.StatusBadRequest))
	defer server.Close()

	cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
		Models: []config.PayloadModelRule{{Name: "*", Protocol: kiroProtocol}},
		Params: map[string]any{
			"conversationState.currentMessage.userInputMessage.modelId": "payload-model",
			"conversationState.currentMessage.userInputMessage.content": "payload-content",
		},
	}}}}
	exec := newKiroTestExecutor(cfg, []kiroEndpointConfig{{Name: "test", URL: server.URL, Origin: "AI_EDITOR"}})

	req := cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-5",
		Payload: kiroClaudePayloadWithThinking(4096),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
		Headers:      http.Header{"X-Kiro-Test": []string{"on"}},
		Metadata:     map[string]any{cliproxyexecutor.RequestPathMetadataKey: "/v1/messages"},
	}
	if err := runKiroExecute(t, exec, req, opts); err == nil {
		t.Fatalf("expected upstream status error from fixture")
	}

	bodies, _ := capture.snapshot()
	if len(bodies) != 1 {
		t.Fatalf("expected exactly one wire attempt, got %d", len(bodies))
	}
	wire := bodies[0]
	if got := gjson.GetBytes(wire, "conversationState.currentMessage.userInputMessage.modelId").String(); got != "payload-model" {
		t.Fatalf("wire modelId = %q, want payload-model (gate did not target final Kiro JSON)", got)
	}
	if got := gjson.GetBytes(wire, "conversationState.currentMessage.userInputMessage.content").String(); got != "payload-content" {
		t.Fatalf("wire content = %q, want payload-content", got)
	}
	// The source-shaped temperature must not leak into the Kiro envelope.
	if gjson.GetBytes(wire, "temperature").Exists() {
		t.Fatalf("source-shaped field leaked into Kiro payload: %s", wire)
	}
}

// TestKiroThinkingBudgetPreservedFromSourcePayload proves a caller-supplied
// thinking budget survives conversion instead of the old hardcoded 16000.
func TestKiroThinkingBudgetPreservedFromSourcePayload(t *testing.T) {
	stubKiroHumanLikeDelay(t)
	capture := &kiroWireCapture{}
	server := httptest.NewServer(capture.handler(http.StatusBadRequest))
	defer server.Close()

	exec := newKiroTestExecutor(&config.Config{}, []kiroEndpointConfig{{Name: "test", URL: server.URL, Origin: "AI_EDITOR"}})
	req := cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-5",
		Payload: kiroClaudePayloadWithThinking(4096),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}
	if err := runKiroExecute(t, exec, req, opts); err == nil {
		t.Fatalf("expected upstream status error from fixture")
	}

	bodies, _ := capture.snapshot()
	if len(bodies) != 1 {
		t.Fatalf("expected exactly one wire attempt, got %d", len(bodies))
	}
	content := gjson.GetBytes(bodies[0], "conversationState.currentMessage.userInputMessage.content").String()
	want := "<max_thinking_length>4096</max_thinking_length>"
	if !strings.Contains(content, want) {
		t.Fatalf("wire thinking budget not preserved: content missing %q", want)
	}
	hardcoded := "<max_thinking_length>" + strconv.FormatInt(common.DefaultThinkingBudget, 10) + "</max_thinking_length>"
	if strings.Contains(content, hardcoded) {
		t.Fatalf("wire thinking budget still hardcoded: content contains %q", hardcoded)
	}
}

// TestKiroPayloadGateAppliedOnEveryEndpointAttempt proves each retry/fallback
// wire attempt is gated once on its own rebuilt Kiro payload.
func TestKiroPayloadGateAppliedOnEveryEndpointAttempt(t *testing.T) {
	stubKiroHumanLikeDelay(t)
	firstCapture := &kiroWireCapture{}
	first := httptest.NewServer(firstCapture.handler(http.StatusTooManyRequests))
	defer first.Close()
	secondCapture := &kiroWireCapture{}
	second := httptest.NewServer(secondCapture.handler(http.StatusBadRequest))
	defer second.Close()

	cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
		Models: []config.PayloadModelRule{{Name: "*", Protocol: kiroProtocol}},
		Params: map[string]any{
			"conversationState.currentMessage.userInputMessage.modelId": "payload-model",
		},
	}}}}
	exec := newKiroTestExecutor(cfg, []kiroEndpointConfig{
		{Name: "first", URL: first.URL, Origin: "AI_EDITOR"},
		{Name: "second", URL: second.URL, Origin: "CLI"},
	})

	req := cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-5",
		Payload: kiroClaudePayloadWithThinking(4096),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}
	if err := runKiroExecute(t, exec, req, opts); err == nil {
		t.Fatalf("expected upstream status error from fixture")
	}

	firstBodies, firstOrigins := firstCapture.snapshot()
	secondBodies, secondOrigins := secondCapture.snapshot()
	if len(firstBodies) != 1 || len(secondBodies) != 1 {
		t.Fatalf("expected one attempt per endpoint, got first=%d second=%d", len(firstBodies), len(secondBodies))
	}
	for i, body := range append(append([][]byte(nil), firstBodies...), secondBodies...) {
		if got := gjson.GetBytes(body, "conversationState.currentMessage.userInputMessage.modelId").String(); got != "payload-model" {
			t.Fatalf("attempt %d not gated after rebuild: modelId = %q", i, got)
		}
	}
	if len(firstOrigins) != 1 || firstOrigins[0] != "AI_EDITOR" {
		t.Fatalf("first attempt origin = %v, want AI_EDITOR", firstOrigins)
	}
	if len(secondOrigins) != 1 || secondOrigins[0] != "CLI" {
		t.Fatalf("second attempt origin = %v, want CLI", secondOrigins)
	}
}

// runKiroExecuteWithAuth is runKiroExecute with a caller-provided auth record so
// tests can exercise auth attributes.
func runKiroExecuteWithAuth(t *testing.T, exec *KiroExecutor, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := exec.Execute(ctx, auth, req, opts)
	return err
}

// TestKiroAgentTaskTypeFromAuthAttributes proves the native builder honors the
// auth-provided agent_task_type instead of hardcoding "vibe".
func TestKiroAgentTaskTypeFromAuthAttributes(t *testing.T) {
	stubKiroHumanLikeDelay(t)
	capture := &kiroWireCapture{}
	server := httptest.NewServer(capture.handler(http.StatusBadRequest))
	defer server.Close()

	exec := newKiroTestExecutor(&config.Config{}, []kiroEndpointConfig{{Name: "test", URL: server.URL, Origin: "AI_EDITOR"}})
	auth := newKiroTestAuth(fmt.Sprintf("test-token-%s-%d", t.Name(), atomic.AddInt64(&kiroTestTokenSeq, 1)))
	auth.Attributes = map[string]string{"agent_task_type": "dev"}

	req := cliproxyexecutor.Request{Model: "claude-sonnet-4-5", Payload: kiroClaudePayloadWithThinking(4096)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}
	if err := runKiroExecuteWithAuth(t, exec, auth, req, opts); err == nil {
		t.Fatalf("expected upstream status error from fixture")
	}

	bodies, _ := capture.snapshot()
	if len(bodies) != 1 {
		t.Fatalf("expected exactly one wire attempt, got %d", len(bodies))
	}
	if got := gjson.GetBytes(bodies[0], "conversationState.agentTaskType").String(); got != "dev" {
		t.Fatalf("wire agentTaskType = %q, want dev", got)
	}
}

// TestKiroAgentTaskTypeInvalidFallsBackToDefault proves unsupported values are
// validated before they reach the Kiro protocol.
func TestKiroAgentTaskTypeInvalidFallsBackToDefault(t *testing.T) {
	stubKiroHumanLikeDelay(t)
	capture := &kiroWireCapture{}
	server := httptest.NewServer(capture.handler(http.StatusBadRequest))
	defer server.Close()

	exec := newKiroTestExecutor(&config.Config{}, []kiroEndpointConfig{{Name: "test", URL: server.URL, Origin: "AI_EDITOR"}})
	auth := newKiroTestAuth(fmt.Sprintf("test-token-%s-%d", t.Name(), atomic.AddInt64(&kiroTestTokenSeq, 1)))
	auth.Attributes = map[string]string{"agent_task_type": "bogus"}

	req := cliproxyexecutor.Request{Model: "claude-sonnet-4-5", Payload: kiroClaudePayloadWithThinking(4096)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}
	if err := runKiroExecuteWithAuth(t, exec, auth, req, opts); err == nil {
		t.Fatalf("expected upstream status error from fixture")
	}

	bodies, _ := capture.snapshot()
	if len(bodies) != 1 {
		t.Fatalf("expected exactly one wire attempt, got %d", len(bodies))
	}
	if got := gjson.GetBytes(bodies[0], "conversationState.agentTaskType").String(); got != common.DefaultAgentTaskType {
		t.Fatalf("wire agentTaskType = %q, want %q", got, common.DefaultAgentTaskType)
	}
}

// TestKiroAgentTaskTypeGateOverrideWins proves the payload gate still runs last:
// config can override the auth-provided task type on the final Kiro JSON.
func TestKiroAgentTaskTypeGateOverrideWins(t *testing.T) {
	stubKiroHumanLikeDelay(t)
	capture := &kiroWireCapture{}
	server := httptest.NewServer(capture.handler(http.StatusBadRequest))
	defer server.Close()

	cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
		Models: []config.PayloadModelRule{{Name: "*", Protocol: kiroProtocol}},
		Params: map[string]any{"conversationState.agentTaskType": "chat"},
	}}}}
	exec := newKiroTestExecutor(cfg, []kiroEndpointConfig{{Name: "test", URL: server.URL, Origin: "AI_EDITOR"}})
	auth := newKiroTestAuth(fmt.Sprintf("test-token-%s-%d", t.Name(), atomic.AddInt64(&kiroTestTokenSeq, 1)))
	auth.Attributes = map[string]string{"agent_task_type": "dev"}

	req := cliproxyexecutor.Request{Model: "claude-sonnet-4-5", Payload: kiroClaudePayloadWithThinking(4096)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}
	if err := runKiroExecuteWithAuth(t, exec, auth, req, opts); err == nil {
		t.Fatalf("expected upstream status error from fixture")
	}

	bodies, _ := capture.snapshot()
	if len(bodies) != 1 {
		t.Fatalf("expected exactly one wire attempt, got %d", len(bodies))
	}
	if got := gjson.GetBytes(bodies[0], "conversationState.agentTaskType").String(); got != "chat" {
		t.Fatalf("wire agentTaskType = %q, want chat (gate must run after the builder)", got)
	}
}

// TestKiroPreferredEndpointFallsBackToConfig proves the config field is not dead:
// when the auth record has no preferred_endpoint, the executor honors
// config.KiroPreferredEndpoint and still lets auth attributes win.
func TestKiroPreferredEndpointFallsBackToConfig(t *testing.T) {
	auth := newKiroTestAuth("test-token-preferred-endpoint")
	cfg := &config.Config{KiroPreferredEndpoint: "codewhisperer"}

	configs := getKiroEndpointConfigs(auth, cfg)
	if len(configs) == 0 || configs[0].Name != "CodeWhisperer" {
		t.Fatalf("first endpoint = %+v, want CodeWhisperer from config", configs)
	}

	auth.Attributes = map[string]string{"preferred_endpoint": "amazonq"}
	configs = getKiroEndpointConfigs(auth, cfg)
	if len(configs) == 0 || configs[0].Name != "AmazonQ" {
		t.Fatalf("first endpoint = %+v, want AmazonQ from auth attribute", configs)
	}
}
