package cliproxy

import (
	"fmt"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// Plus providers must use their native protocol implementations, not the generic
// OpenAI-compatible fallback used for unknown providers.
func TestPlusProviderNativeExecutorRegistration(t *testing.T) {
	cases := []struct{ provider, executorType string }{
		{"kiro", "*executor.KiroExecutor"},
		{"kilo", "*executor.KiloExecutor"},
		{"cursor", "*executor.CursorExecutor"},
		{"github-copilot", "*executor.GitHubCopilotExecutor"},
		{"codebuddy", "*executor.CodeBuddyExecutor"},
		{"codebuddy-ai", "*executor.CodeBuddyAIExecutor"},
		{"codearts", "*executor.CodeArtsExecutor"},
		{"joycode", "*executor.JoyCodeExecutor"},
		{"gitlab", "*executor.GitLabExecutor"},
		{"bt", "*executor.BTExecutor"},
		{"qoder", "*executor.QoderExecutor"},
		{"iflow", "*executor.IFlowExecutor"},
	}
	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			service := &Service{cfg: &config.Config{}, coreManager: coreauth.NewManager(nil, nil, nil)}
			service.ensureExecutorsForAuth(&coreauth.Auth{ID: "plus-test", Provider: tc.provider})
			got, ok := service.coreManager.Executor(tc.provider)
			if !ok || got == nil {
				t.Fatalf("native executor for %s was not registered", tc.provider)
			}
			if gotType := fmt.Sprintf("%T", got); gotType != tc.executorType {
				t.Fatalf("executor for %s = %s, want %s", tc.provider, gotType, tc.executorType)
			}
		})
	}
}
