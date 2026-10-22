package config

import "strings"

// KiroKey configures an AWS CodeWhisperer or Amazon Q credential.
type KiroKey struct {
	TokenFile         string `yaml:"token-file,omitempty" json:"token-file,omitempty"`
	AccessToken       string `yaml:"access-token,omitempty" json:"access-token,omitempty"`
	RefreshToken      string `yaml:"refresh-token,omitempty" json:"refresh-token,omitempty"`
	ProfileArn        string `yaml:"profile-arn,omitempty" json:"profile-arn,omitempty"`
	Region            string `yaml:"region,omitempty" json:"region,omitempty"`
	StartURL          string `yaml:"start-url,omitempty" json:"start-url,omitempty"`
	ProxyURL          string `yaml:"proxy-url,omitempty" json:"proxy-url,omitempty"`
	AgentTaskType     string `yaml:"agent-task-type,omitempty" json:"agent-task-type,omitempty"`
	PreferredEndpoint string `yaml:"preferred-endpoint,omitempty" json:"preferred-endpoint,omitempty"`
}

// BTKey configures a BaoTa account. Base64 encoding does not encrypt Password.
type BTKey struct {
	Phone          string                     `yaml:"phone" json:"phone"`
	Password       string                     `yaml:"password" json:"password"`
	Prefix         string                     `yaml:"prefix,omitempty" json:"prefix,omitempty"`
	ProxyURL       string                     `yaml:"proxy-url,omitempty" json:"proxy-url,omitempty"`
	Models         []OpenAICompatibilityModel `yaml:"models,omitempty" json:"models,omitempty"`
	ExcludedModels []string                   `yaml:"excluded-models,omitempty" json:"excluded-models,omitempty"`
	Priority       int                        `yaml:"priority,omitempty" json:"priority,omitempty"`
	Headers        map[string]string          `yaml:"headers,omitempty" json:"headers,omitempty"`
}

// KiroFingerprintConfig optionally pins Kiro request identity fields.
type KiroFingerprintConfig struct {
	OIDCSDKVersion      string `yaml:"oidc-sdk-version,omitempty" json:"oidc-sdk-version,omitempty"`
	RuntimeSDKVersion   string `yaml:"runtime-sdk-version,omitempty" json:"runtime-sdk-version,omitempty"`
	StreamingSDKVersion string `yaml:"streaming-sdk-version,omitempty" json:"streaming-sdk-version,omitempty"`
	OSType              string `yaml:"os-type,omitempty" json:"os-type,omitempty"`
	OSVersion           string `yaml:"os-version,omitempty" json:"os-version,omitempty"`
	NodeVersion         string `yaml:"node-version,omitempty" json:"node-version,omitempty"`
	KiroVersion         string `yaml:"kiro-version,omitempty" json:"kiro-version,omitempty"`
	KiroHash            string `yaml:"kiro-hash,omitempty" json:"kiro-hash,omitempty"`
}

// SanitizeKiroKeys trims whitespace from provider credential fields.
func (cfg *Config) SanitizeKiroKeys() {
	if cfg == nil {
		return
	}
	for i := range cfg.KiroKey {
		entry := &cfg.KiroKey[i]
		for _, field := range []*string{&entry.TokenFile, &entry.AccessToken, &entry.RefreshToken, &entry.ProfileArn, &entry.Region, &entry.StartURL, &entry.ProxyURL, &entry.AgentTaskType, &entry.PreferredEndpoint} {
			*field = strings.TrimSpace(*field)
		}
	}
	cfg.KiroPreferredEndpoint = strings.TrimSpace(cfg.KiroPreferredEndpoint)
}

// SanitizeBTKeys normalizes BaoTa credentials and drops incomplete entries.
func (cfg *Config) SanitizeBTKeys() {
	if cfg == nil {
		return
	}
	out := make([]BTKey, 0, len(cfg.BTKey))
	for _, entry := range cfg.BTKey {
		entry.Phone = strings.TrimSpace(entry.Phone)
		entry.Password = strings.TrimSpace(entry.Password)
		entry.Prefix = normalizeModelPrefix(entry.Prefix)
		entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
		entry.Headers = NormalizeHeaders(entry.Headers)
		entry.ExcludedModels = NormalizeExcludedModels(entry.ExcludedModels)
		if entry.Phone == "" || entry.Password == "" {
			continue
		}
		out = append(out, entry)
	}
	cfg.BTKey = out
}
