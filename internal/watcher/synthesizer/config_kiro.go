package synthesizer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/kiro"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/constant"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// synthesizeKiroKeys creates Auth entries for Kiro config credentials.
//
// The synthesizer is pure: it only reads local token files and never performs
// network calls. Config entries that only carry a refresh token are still
// synthesized; the runtime refresh path (or the credential activator) is
// responsible for turning them into a usable access token.
func (s *ConfigSynthesizer) synthesizeKiroKeys(ctx *SynthesisContext) []*coreauth.Auth {
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0, len(ctx.Config.KiroKey))
	for i := range ctx.Config.KiroKey {
		entry := ctx.Config.KiroKey[i]
		tokenData, errLoad := loadKiroConfigToken(entry)
		if errLoad != nil {
			log.WithError(errLoad).Warnf("kiro config credential %d: token file could not be loaded, falling back to inline fields", i)
		}
		if strings.TrimSpace(tokenData.AccessToken) == "" && strings.TrimSpace(tokenData.RefreshToken) == "" {
			log.Warnf("skipping kiro config credential %d: token file and inline fields carry no access or refresh token", i)
			continue
		}

		identity := []string{
			strings.TrimSpace(tokenData.ProfileArn),
			strings.TrimSpace(tokenData.Region),
			strings.TrimSpace(tokenData.StartURL),
			strings.TrimSpace(tokenData.Email),
			strings.TrimSpace(entry.TokenFile),
		}
		if strings.TrimSpace(identity[0]+identity[1]+identity[2]+identity[3]+identity[4]) == "" {
			// No stable identity fields: derive the ID from the credential material
			// so the ID stays deterministic without exposing the secret itself
			// (the generator hashes its inputs).
			identity = append(identity, tokenData.AccessToken, tokenData.RefreshToken, tokenData.ClientIDHash)
		}
		id, token := idGen.Next("kiro:config", identity...)

		proxyURL := strings.TrimSpace(entry.ProxyURL)
		attributes := map[string]string{
			"source":       fmt.Sprintf("config:kiro[%s]", token),
			"config_index": strconv.Itoa(i),
		}
		for key, value := range map[string]string{
			"profile_arn": strings.TrimSpace(tokenData.ProfileArn),
			"region":      strings.TrimSpace(tokenData.Region),
			"start_url":   strings.TrimSpace(tokenData.StartURL),
			"email":       strings.TrimSpace(tokenData.Email),
		} {
			if value != "" {
				attributes[key] = value
			}
		}
		preferredEndpoint := strings.TrimSpace(entry.PreferredEndpoint)
		if preferredEndpoint == "" {
			preferredEndpoint = strings.TrimSpace(ctx.Config.KiroPreferredEndpoint)
		}
		if preferredEndpoint != "" {
			attributes["preferred_endpoint"] = preferredEndpoint
		}
		if taskType := strings.TrimSpace(entry.AgentTaskType); taskType != "" {
			attributes["agent_task_type"] = taskType
		}

		metadata := map[string]any{
			"type": constant.Kiro,
		}
		for key, value := range map[string]string{
			"access_token":   strings.TrimSpace(tokenData.AccessToken),
			"refresh_token":  strings.TrimSpace(tokenData.RefreshToken),
			"profile_arn":    strings.TrimSpace(tokenData.ProfileArn),
			"expires_at":     strings.TrimSpace(tokenData.ExpiresAt),
			"auth_method":    strings.TrimSpace(tokenData.AuthMethod),
			"provider":       strings.TrimSpace(tokenData.Provider),
			"client_id":      strings.TrimSpace(tokenData.ClientID),
			"client_secret":  strings.TrimSpace(tokenData.ClientSecret),
			"client_id_hash": strings.TrimSpace(tokenData.ClientIDHash),
			"email":          strings.TrimSpace(tokenData.Email),
			"start_url":      strings.TrimSpace(tokenData.StartURL),
			"region":         strings.TrimSpace(tokenData.Region),
		} {
			if value != "" {
				metadata[key] = value
			}
		}
		if preferredEndpoint != "" {
			metadata["preferred_endpoint"] = preferredEndpoint
		}

		attributes[KiroConfigFingerprintAttr] = kiroFingerprint(
			strings.TrimSpace(entry.TokenFile),
			strings.TrimSpace(entry.ProfileArn),
			strings.TrimSpace(entry.Region),
			strings.TrimSpace(entry.StartURL),
			strings.TrimSpace(entry.ProxyURL),
			strings.TrimSpace(entry.AgentTaskType),
			preferredEndpoint,
		)
		attributes[KiroTokenFingerprintAttr] = kiroFingerprint(
			strings.TrimSpace(tokenData.AccessToken),
			strings.TrimSpace(tokenData.RefreshToken),
			strings.TrimSpace(tokenData.ClientID),
			strings.TrimSpace(tokenData.ClientSecret),
			strings.TrimSpace(tokenData.ClientIDHash),
			strings.TrimSpace(tokenData.ExpiresAt),
			strings.TrimSpace(tokenData.AuthMethod),
			strings.TrimSpace(tokenData.Provider),
		)

		auth := &coreauth.Auth{
			ID:         id,
			Provider:   constant.Kiro,
			Label:      "kiro-config",
			Status:     coreauth.StatusActive,
			ProxyURL:   proxyURL,
			Attributes: attributes,
			Metadata:   metadata,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		applyKiroNextRefreshAfter(auth, tokenData, now)
		out = append(out, auth)
	}
	return out
}

// loadKiroConfigToken merges an optional token file with inline config fields.
// Inline fields win when both sources carry a value.
func loadKiroConfigToken(entry config.KiroKey) (kiro.KiroTokenData, error) {
	var tokenData kiro.KiroTokenData
	var errLoad error
	if path := strings.TrimSpace(entry.TokenFile); path != "" {
		expanded, errExpand := expandKiroTokenPath(path)
		if errExpand != nil {
			errLoad = errExpand
		} else if raw, errRead := os.ReadFile(expanded); errRead != nil {
			errLoad = fmt.Errorf("read %s: %w", expanded, errRead)
		} else if errUnmarshal := json.Unmarshal(raw, &tokenData); errUnmarshal != nil {
			errLoad = fmt.Errorf("decode %s: %w", expanded, errUnmarshal)
		}
	}
	if value := strings.TrimSpace(entry.AccessToken); value != "" {
		tokenData.AccessToken = value
	}
	if value := strings.TrimSpace(entry.RefreshToken); value != "" {
		tokenData.RefreshToken = value
	}
	if value := strings.TrimSpace(entry.ProfileArn); value != "" {
		tokenData.ProfileArn = value
	}
	if value := strings.TrimSpace(entry.Region); value != "" {
		tokenData.Region = value
	}
	if value := strings.TrimSpace(entry.StartURL); value != "" {
		tokenData.StartURL = value
	}
	return tokenData, errLoad
}

func expandKiroTokenPath(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		home, errHome := os.UserHomeDir()
		if errHome != nil {
			return "", fmt.Errorf("resolve home directory for %s: %w", path, errHome)
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}

// applyKiroNextRefreshAfter schedules the first proactive refresh from the token
// expiry. Credentials that carry only a refresh token are refreshed immediately
// so they become usable without waiting for the periodic refresh interval.
func applyKiroNextRefreshAfter(auth *coreauth.Auth, tokenData kiro.KiroTokenData, now time.Time) {
	if expiresAt := strings.TrimSpace(tokenData.ExpiresAt); expiresAt != "" {
		if parsed, errParse := time.Parse(time.RFC3339, expiresAt); errParse == nil {
			next := parsed.Add(-20 * time.Minute)
			if next.Before(now) {
				next = now
			}
			auth.NextRefreshAfter = next
			return
		}
	}
	if strings.TrimSpace(tokenData.AccessToken) == "" && strings.TrimSpace(tokenData.RefreshToken) != "" {
		auth.NextRefreshAfter = now
	}
}

const (
	// KiroConfigFingerprintAttr fingerprints the config inputs that do not
	// carry the credential secret itself.
	KiroConfigFingerprintAttr = "kiro_config_fingerprint"
	// KiroTokenFingerprintAttr fingerprints the token material read from the
	// config or the token file.
	KiroTokenFingerprintAttr = "kiro_token_fingerprint"
)

// kiroFingerprint hashes the supplied values with separators so distinct
// field combinations cannot collide by concatenation.
func kiroFingerprint(values ...string) string {
	h := sha256.New()
	for _, value := range values {
		_, _ = io.WriteString(h, value)
		_, _ = io.WriteString(h, "\x00")
	}
	return hex.EncodeToString(h.Sum(nil))
}
