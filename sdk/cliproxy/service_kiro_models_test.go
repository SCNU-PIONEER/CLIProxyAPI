package cliproxy

import (
	"testing"

	kiroauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/kiro"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestKiroModelConversionAndAgenticVariants(t *testing.T) {
	models := convertKiroAPIModels([]*kiroauth.KiroModel{nil, {}, {ModelID: "anthropic.claude_sonnet", ModelName: "Sonnet", MaxInputTokens: 100000, RateMultiplier: 2}})
	if len(models) != 1 || models[0].ID != "kiro-claude-sonnet" || models[0].ContextLength != 100000 || models[0].DisplayName != "Kiro Sonnet (2.0x credit)" {
		t.Fatalf("unexpected converted models: %+v", models)
	}
	models = append(models, &ModelInfo{ID: "kiro-auto"})
	variants := generateKiroAgenticVariants(models)
	if len(variants) != 3 || variants[2].ID != "kiro-claude-sonnet-agentic" {
		t.Fatalf("unexpected variants: %+v", variants)
	}
	variants[2].Thinking.Min = 1
	if models[0].Thinking.Min == 1 {
		t.Fatal("variant shares mutable thinking config")
	}
}

func TestKiroTokenAttributesTakePrecedence(t *testing.T) {
	s := &Service{}
	a := &coreauth.Auth{Attributes: map[string]string{"access_token": " configured "}, Metadata: map[string]any{"access_token": "file"}}
	if got := s.extractKiroTokenData(a); got == nil || got.AccessToken != "configured" {
		t.Fatalf("unexpected token: %#v", got)
	}
	delete(a.Attributes, "access_token")
	if got := s.extractKiroTokenData(a); got == nil || got.AccessToken != "file" {
		t.Fatalf("missing metadata fallback: %#v", got)
	}
	if s.extractKiroTokenData(nil) != nil {
		t.Fatal("nil auth should not produce token data")
	}
}
