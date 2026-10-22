package claude

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/translator/translator"
)

// TestClaudeToKiroRegistration verifies that the v8 registration adapter
// exposes the legacy Claude->Kiro converter through the registry, returns no
// error, and preserves the converter output.
func TestClaudeToKiroRegistration(t *testing.T) {
	payload := []byte(`{"model":"claude-sonnet-4","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`)

	got, err := convertClaudeRequestToKiro("claude-sonnet-4", payload, false)
	if err != nil {
		t.Fatalf("adapter error = %v, want nil", err)
	}
	if len(got) == 0 {
		t.Fatal("adapter returned an empty payload")
	}
	if !json.Valid(got) {
		t.Fatalf("adapter output is not valid JSON: %s", got)
	}

	registered := translator.Request(constant.Claude, constant.Kiro, "claude-sonnet-4", payload, false)
	if len(registered) == 0 {
		t.Fatal("registered Claude->Kiro transform returned an empty payload")
	}
	if string(registered) != string(got) {
		t.Fatalf("registered transform output differs from the converter output")
	}
}
