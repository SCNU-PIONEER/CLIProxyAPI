package openai

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/translator/translator"
)

// TestOpenAIToKiroRegistration verifies that the v8 registration adapter
// exposes the legacy OpenAI->Kiro converter through the registry, returns no
// error, and preserves the converter output.
func TestOpenAIToKiroRegistration(t *testing.T) {
	payload := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hello"}]}`)

	got, err := convertOpenAIRequestToKiro("gpt-5", payload, false)
	if err != nil {
		t.Fatalf("adapter error = %v, want nil", err)
	}
	if len(got) == 0 {
		t.Fatal("adapter returned an empty payload")
	}
	if !json.Valid(got) {
		t.Fatalf("adapter output is not valid JSON: %s", got)
	}

	registered := translator.Request(constant.OpenAI, constant.Kiro, "gpt-5", payload, false)
	if len(registered) == 0 {
		t.Fatal("registered OpenAI->Kiro transform returned an empty payload")
	}
	if string(registered) != string(got) {
		t.Fatalf("registered transform output differs from the converter output")
	}
}
