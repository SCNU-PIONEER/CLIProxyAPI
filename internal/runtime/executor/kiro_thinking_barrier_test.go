package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func kiroClaudePayloadWithoutThinking() []byte {
	return []byte(`{"model":"claude-sonnet-4-5","max_tokens":1024,"system":"base system","messages":[{"role":"user","content":"hello"}]}`)
}

// TestKiroSuffixThinkingBudgetReachesFinalPayload proves the canonical thinking
// step runs against the intermediate Claude body (the Kiro translators pass it
// through unchanged) and that the suffix-derived budget reaches the final Kiro
// wire JSON instead of being dropped by the missing "kiro" thinking applier.
func TestKiroSuffixThinkingBudgetReachesFinalPayload(t *testing.T) {
	stubKiroHumanLikeDelay(t)
	capture := &kiroWireCapture{}
	server := httptest.NewServer(capture.handler(http.StatusBadRequest))
	defer server.Close()

	exec := newKiroTestExecutor(&config.Config{}, []kiroEndpointConfig{{Name: "test", URL: server.URL, Origin: "AI_EDITOR"}})
	req := cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-5(24576)",
		Payload: kiroClaudePayloadWithoutThinking(),
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
	want := "<max_thinking_length>24576</max_thinking_length>"
	if !strings.Contains(content, want) {
		t.Fatalf("wire thinking budget from suffix missing: content does not contain %q: %s", want, content)
	}
}

// TestKiroFinalPayloadRuleWinsOverBuiltThinking proves the payload gate still
// runs last: a config rule can overwrite the builder-provided content on the
// final native Kiro JSON.
func TestKiroFinalPayloadRuleWinsOverBuiltThinking(t *testing.T) {
	stubKiroHumanLikeDelay(t)
	capture := &kiroWireCapture{}
	server := httptest.NewServer(capture.handler(http.StatusBadRequest))
	defer server.Close()

	cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
		Models: []config.PayloadModelRule{{Name: "*", Protocol: kiroProtocol}},
		Params: map[string]any{
			"conversationState.currentMessage.userInputMessage.content": "payload-content",
		},
	}}}}
	exec := newKiroTestExecutor(cfg, []kiroEndpointConfig{{Name: "test", URL: server.URL, Origin: "AI_EDITOR"}})
	req := cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-5(high)",
		Payload: kiroClaudePayloadWithoutThinking(),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}
	if err := runKiroExecute(t, exec, req, opts); err == nil {
		t.Fatalf("expected upstream status error from fixture")
	}

	bodies, _ := capture.snapshot()
	if len(bodies) != 1 {
		t.Fatalf("expected exactly one wire attempt, got %d", len(bodies))
	}
	if got := gjson.GetBytes(bodies[0], "conversationState.currentMessage.userInputMessage.content").String(); got != "payload-content" {
		t.Fatalf("wire content = %q, want payload-content (gate must run after the builder)", got)
	}
}

var _ = context.Background
var _ = cliproxyauth.Auth{}
