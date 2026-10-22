package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// These tests pin the final barrier of the GitLab text gateway: payload rules
// must target the native GitLab wire body (built from gitLabPrompt), not the
// intermediate OpenAI-shaped JSON, and header-scoped rules must receive the
// client headers.
func gitLabWireGateConfig() *config.Config {
	return &config.Config{
		Payload: config.PayloadConfig{
			Override: []config.PayloadRule{
				{
					Models: []config.PayloadModelRule{
						{
							Name:     "*",
							Protocol: gitLabWireProtocol,
							Headers:  map[string]string{"X-Payload-Gate": "on"},
						},
					},
					Params: map[string]any{
						"content":                           "gated-content",
						"user_instruction":                  "gated-instruction",
						"current_file.file_name":            "gated.go",
						"current_file.content_above_cursor": "gated-above",
					},
				},
			},
		},
	}
}

func newGitLabGateAuth(baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		Provider: "gitlab",
		Metadata: map[string]any{
			"base_url":     baseURL,
			"access_token": "oauth-access",
			"model_name":   "claude-sonnet-4-5",
		},
	}
}

func gitLabGateOptions(header http.Header, stream bool) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       stream,
		Headers:      header,
		Metadata:     map[string]any{cliproxyexecutor.RequestPathMetadataKey: "/v1/chat/completions"},
	}
}

func gitLabGateRequest() cliproxyexecutor.Request {
	return cliproxyexecutor.Request{
		Model:   "gitlab-duo",
		Payload: []byte(`{"model":"gitlab-duo","messages":[{"role":"user","content":"hello"}]}`),
	}
}

// TestGitLabExecuteAppliesFinalWirePayloadGate proves Execute gates the chat
// body actually sent to GitLab.
func TestGitLabExecuteAppliesFinalWirePayloadGate(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = readBody(t, r)
		_, _ = w.Write([]byte(`"ok"`))
	}))
	defer srv.Close()

	exec := NewGitLabExecutor(gitLabWireGateConfig())
	auth := newGitLabGateAuth(srv.URL)
	if _, err := exec.Execute(context.Background(), auth, gitLabGateRequest(), gitLabGateOptions(http.Header{"X-Payload-Gate": []string{"on"}}, false)); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := gjson.GetBytes(captured, "content").String(); got != "gated-content" {
		t.Fatalf("wire content = %q, want gated-content (gate must target the final GitLab body); body=%s", got, captured)
	}
}

// TestGitLabExecuteWirePayloadGateRequiresMatchingHeader proves header-scoped
// rules are evaluated against the client headers, not invented ones.
func TestGitLabExecuteWirePayloadGateRequiresMatchingHeader(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = readBody(t, r)
		_, _ = w.Write([]byte(`"ok"`))
	}))
	defer srv.Close()

	exec := NewGitLabExecutor(gitLabWireGateConfig())
	auth := newGitLabGateAuth(srv.URL)
	if _, err := exec.Execute(context.Background(), auth, gitLabGateRequest(), gitLabGateOptions(nil, false)); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := gjson.GetBytes(captured, "content").String(); got == "gated-content" {
		t.Fatalf("header-scoped rule must not match without the header; body=%s", captured)
	}
}

// TestGitLabCodeSuggestionsWirePayloadGate proves the fallback text body is
// gated on its own native shape.
func TestGitLabCodeSuggestionsWirePayloadGate(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == gitLabChatEndpoint {
			http.Error(w, "feature unavailable", http.StatusForbidden)
			return
		}
		captured = readBody(t, r)
		_, _ = w.Write([]byte(`"ok"`))
	}))
	defer srv.Close()

	exec := NewGitLabExecutor(gitLabWireGateConfig())
	auth := newGitLabGateAuth(srv.URL)
	if _, err := exec.Execute(context.Background(), auth, gitLabGateRequest(), gitLabGateOptions(http.Header{"X-Payload-Gate": []string{"on"}}, false)); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := gjson.GetBytes(captured, "user_instruction").String(); got != "gated-instruction" {
		t.Fatalf("wire user_instruction = %q, want gated-instruction; body=%s", got, captured)
	}
	if got := gjson.GetBytes(captured, "current_file.file_name").String(); got != "gated.go" {
		t.Fatalf("wire current_file.file_name = %q, want gated.go; body=%s", got, captured)
	}
}

// TestGitLabCodeSuggestionsStreamWirePayloadGate covers the streaming wire body.
func TestGitLabCodeSuggestionsStreamWirePayloadGate(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = readBody(t, r)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: content_chunk\ndata: {\"delta\":\"hi\"}\n\n"))
	}))
	defer srv.Close()

	exec := NewGitLabExecutor(gitLabWireGateConfig())
	auth := newGitLabGateAuth(srv.URL)
	result, err := exec.ExecuteStream(context.Background(), auth, gitLabGateRequest(), gitLabGateOptions(http.Header{"X-Payload-Gate": []string{"on"}}, true))
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	for range result.Chunks {
	}
	if got := gjson.GetBytes(captured, "user_instruction").String(); got != "gated-instruction" {
		t.Fatalf("stream wire user_instruction = %q, want gated-instruction; body=%s", got, captured)
	}
	if !gjson.GetBytes(captured, "stream").Bool() {
		t.Fatalf("stream wire body must keep stream=true; body=%s", captured)
	}
}
