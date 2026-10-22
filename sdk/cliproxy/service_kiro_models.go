package cliproxy

import (
	"context"
	"fmt"
	"strings"
	"time"

	kiroauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/kiro"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// fetchKiroModels uses the registration caller's context and configuration snapshot.
// Failed discovery retains the embedded catalog; cancellation is checked by the publication fence.
func (s *Service) fetchKiroModels(ctx context.Context, cfg *config.Config, a *coreauth.Auth) []*ModelInfo {
	token := s.extractKiroTokenData(a)
	if token == nil {
		return registry.GetKiroModels()
	}
	apiModels, err := kiroauth.NewKiroAuth(cfg).ListAvailableModels(ctx, token)
	if err != nil {
		return registry.GetKiroModels()
	}
	models := convertKiroAPIModels(apiModels)
	if len(models) == 0 {
		return registry.GetKiroModels()
	}
	return generateKiroAgenticVariants(models)
}

func (s *Service) extractKiroTokenData(a *coreauth.Auth) *kiroauth.KiroTokenData {
	if a == nil {
		return nil
	}
	accessToken := strings.TrimSpace(a.Attributes["access_token"])
	profileArn := strings.TrimSpace(a.Attributes["profile_arn"])
	refreshToken := strings.TrimSpace(a.Attributes["refresh_token"])
	if accessToken == "" {
		accessToken, _ = a.Metadata["access_token"].(string)
		profileArn, _ = a.Metadata["profile_arn"].(string)
		refreshToken, _ = a.Metadata["refresh_token"].(string)
	}
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return nil
	}
	return &kiroauth.KiroTokenData{AccessToken: accessToken, ProfileArn: strings.TrimSpace(profileArn), RefreshToken: strings.TrimSpace(refreshToken)}
}

func convertKiroAPIModels(apiModels []*kiroauth.KiroModel) []*ModelInfo {
	var models []*ModelInfo
	now := time.Now().Unix()
	for _, m := range apiModels {
		if m == nil || strings.TrimSpace(m.ModelID) == "" {
			continue
		}
		info := &ModelInfo{
			ID: "kiro-" + normalizeKiroModelID(m.ModelID), Object: "model", Created: now,
			OwnedBy: "aws", Type: "kiro", DisplayName: formatKiroDisplayName(m.ModelName, m.RateMultiplier),
			Description: m.Description, ContextLength: 200000, MaxCompletionTokens: 64000,
			Thinking: &registry.ThinkingSupport{Min: 1024, Max: 32000, ZeroAllowed: true, DynamicAllowed: true},
		}
		if m.MaxInputTokens > 0 {
			info.ContextLength = m.MaxInputTokens
		}
		models = append(models, info)
	}
	return models
}

func normalizeKiroModelID(id string) string {
	id = strings.TrimPrefix(strings.TrimSpace(id), "anthropic.")
	id = strings.TrimPrefix(id, "amazon.")
	return strings.ToLower(strings.NewReplacer(".", "-", "_", "-").Replace(id))
}

func formatKiroDisplayName(name string, multiplier float64) string {
	if name == "" {
		return ""
	}
	name = "Kiro " + name
	if multiplier > 0 && multiplier != 1 {
		name += fmt.Sprintf(" (%.1fx credit)", multiplier)
	}
	return name
}

func generateKiroAgenticVariants(models []*ModelInfo) []*ModelInfo {
	result := append([]*ModelInfo(nil), models...)
	for _, m := range models {
		if m == nil || strings.HasSuffix(m.ID, "-agentic") || strings.Contains(m.ID, "-auto") {
			continue
		}
		variant := *m
		variant.ID += "-agentic"
		variant.DisplayName += " (Agentic)"
		variant.Description += " - Optimized for coding agents (chunked writes)"
		if m.Thinking != nil {
			thinking := *m.Thinking
			thinking.Levels = append([]string(nil), m.Thinking.Levels...)
			variant.Thinking = &thinking
		}
		result = append(result, &variant)
	}
	return result
}
