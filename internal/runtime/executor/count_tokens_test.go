package executor

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// Providers without a native token-count endpoint used to reject CountTokens.
// They now estimate locally from the normalized request, so clients still get a
// usable count.
func TestCountTokensLocalEstimateForCompatProviders(t *testing.T) {
	req := cliproxyexecutor.Request{
		Model:   "gpt-4o-mini",
		Payload: []byte(`{"model":"gpt-4o-mini","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hello there"}]}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")}
	counters := map[string]func(context.Context) (cliproxyexecutor.Response, error){
		"codearts": func(ctx context.Context) (cliproxyexecutor.Response, error) {
			return NewCodeArtsExecutor(&config.Config{}).CountTokens(ctx, nil, req, opts)
		},
		"joycode": func(ctx context.Context) (cliproxyexecutor.Response, error) {
			return NewJoyCodeExecutor(&config.Config{}).CountTokens(ctx, nil, req, opts)
		},
		"kilo": func(ctx context.Context) (cliproxyexecutor.Response, error) {
			return NewKiloExecutor(&config.Config{}).CountTokens(ctx, nil, req, opts)
		},
	}
	for name, count := range counters {
		t.Run(name, func(t *testing.T) {
			resp, err := count(context.Background())
			if err != nil {
				t.Fatalf("CountTokens() error = %v", err)
			}
			if got := gjson.GetBytes(resp.Payload, "usage.prompt_tokens").Int(); got <= 0 {
				t.Fatalf("usage.prompt_tokens = %d, want > 0; payload=%s", got, resp.Payload)
			}
		})
	}
}
