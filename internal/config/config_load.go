package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

// LoadConfig reads a YAML configuration file from the given path,
// unmarshals it into a Config struct, applies environment variable overrides,
// and returns it.
//
// Parameters:
//   - configFile: The path to the YAML configuration file
//
// Returns:
//   - *Config: The loaded configuration
//   - error: An error if the configuration could not be loaded
func LoadConfig(configFile string) (*Config, error) {
	return LoadConfigOptional(configFile, false)
}

// ComputeSourceHash returns the Config.SourceHash value for config file bytes.
func ComputeSourceHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// PrepareConfigFileForUpload applies the file rewrites of a startup load: a plaintext
// management secret is replaced with its bcrypt hash and conflicting legacy fields are
// removed. Stores that seed remote storage from configFile before the startup load call
// it first, so the plaintext secret never leaves the machine and the local and remote
// copies stay identical. When the plaintext cannot be removed it returns an error and
// leaves configFile untouched, so every later attempt refuses the file again.
func PrepareConfigFileForUpload(configFile string) error {
	errUpdate := UpdateConfigFile(configFile, func(data []byte) ([]byte, error) {
		prepared, errPrepare := prepareConfigForUpload(data)
		if errPrepare != nil {
			return nil, errPrepare
		}
		if bytes.Equal(prepared, data) {
			return data, nil
		}
		return prepared, nil
	})
	if errUpdate != nil {
		return fmt.Errorf("failed to write prepared config file: %w", errUpdate)
	}
	return nil
}

// prepareConfigForUpload returns data with the file rewrites of a startup load applied.
// It fails when the result would still hold a plaintext management secret: hashing
// replaces only the effective key and the layout cleanup only drops an overridden legacy
// key, so a YAML anchor or a copy elsewhere keeps the plaintext, and rewriting those could
// change other settings that share them.
func prepareConfigForUpload(data []byte) ([]byte, error) {
	var cfg Config
	if errParse := yaml.Unmarshal(data, &cfg); errParse != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", errParse)
	}
	prepared := data
	if secret := cfg.RemoteManagement.SecretKey; secret != "" && !looksLikeBcrypt(secret) {
		hashed, errHash := hashSecret(secret)
		if errHash != nil {
			return nil, fmt.Errorf("failed to hash remote management key: %w", errHash)
		}
		updated, errUpdate := updateNestedScalarYAML(data, managementSecretKeyPath(data), hashed)
		if errUpdate != nil {
			return nil, fmt.Errorf("failed to update remote management key: %w", errUpdate)
		}
		prepared = updated
	}
	cleaned, _, errLayout := NormalizeConfigLayout(prepared, false)
	if errLayout != nil {
		return nil, errLayout
	}
	if plaintexts := plaintextManagementSecrets(data, cfg.RemoteManagement.SecretKey); len(plaintexts) > 0 {
		var doc yaml.Node
		if errParse := yaml.Unmarshal(cleaned, &doc); errParse != nil {
			return nil, fmt.Errorf("failed to parse prepared config: %w", errParse)
		}
		for _, plaintext := range plaintexts {
			if yamlScalarExists(&doc, plaintext) {
				return nil, errors.New("plaintext remote management key is still present elsewhere in the config file, for example through a YAML anchor or as another setting's value")
			}
		}
	}
	return cleaned, nil
}

// plaintextManagementSecrets returns the plaintext management secrets of data: the
// effective one and the values of the legacy and v8 secret keys, since an overridden
// legacy key still holds its own. YAML anchors, aliases and merge keys are resolved, so
// data must already decode as a Config, which rejects cyclic aliases.
func plaintextManagementSecrets(data []byte, effective string) []string {
	candidates := []string{effective}
	var doc yaml.Node
	if yaml.Unmarshal(data, &doc) == nil && len(doc.Content) > 0 {
		root := expandConfigAliases(doc.Content[0])
		for _, path := range []string{"remote-management.secret-key", "management.secret-key"} {
			if node := yamlPath(root, path); node != nil && node.Kind == yaml.ScalarNode && node.ShortTag() != "!!null" {
				candidates = append(candidates, node.Value)
			}
		}
	}
	var plaintexts []string
	for _, candidate := range candidates {
		if candidate != "" && !looksLikeBcrypt(candidate) {
			plaintexts = append(plaintexts, candidate)
		}
	}
	return plaintexts
}

// yamlScalarExists reports whether node or any value below it is a non-null scalar equal
// to value. Mapping keys only name settings and are skipped, as are alias nodes, whose
// anchors are visited where they are defined.
func yamlScalarExists(node *yaml.Node, value string) bool {
	if node.Kind == yaml.ScalarNode {
		return node.ShortTag() != "!!null" && node.Value == value
	}
	first, step := 0, 1
	if node.Kind == yaml.MappingNode {
		first, step = 1, 2
	}
	for i := first; i < len(node.Content); i += step {
		if yamlScalarExists(node.Content[i], value) {
			return true
		}
	}
	return false
}

// managementSecretKeyPath returns the management secret key path used by data: the v8
// path when the document defines it, the legacy path otherwise.
func managementSecretKeyPath(data []byte) []string {
	var source yaml.Node
	if yaml.Unmarshal(data, &source) == nil && len(source.Content) > 0 && yamlPath(expandConfigAliases(source.Content[0]), "management.secret-key") != nil {
		return []string{"management", "secret-key"}
	}
	return []string{"remote-management", "secret-key"}
}

// LoadConfigOptional reads YAML from configFile.
// If optional is true and the file is missing, it returns an empty Config.
// If optional is true and the file is empty or invalid, it returns an empty Config.
func LoadConfigOptional(configFile string, optional bool) (*Config, error) {
	// Read the entire configuration file into memory. The shared lock keeps a
	// reload from observing a truncate window from an in-process management
	// save; the lock is released before parsing below.
	data, err := ReadConfigFile(configFile)
	if err != nil {
		if optional {
			if os.IsNotExist(err) || errors.Is(err, syscall.EISDIR) {
				// Missing and optional: return empty config (cloud deploy standby).
				cfg := &Config{CredentialInFlight: DefaultCredentialInFlightConfig()}
				cfg.NormalizePluginsConfig()
				cfg.NormalizeClaudeConfig()
				return cfg, nil
			}
		}
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}
	return loadConfigOptionalBytes(data, optional, configFile)
}

// loadConfigOptionalBytes is the shared parse/normalize/validate core for
// LoadConfigOptional and ParseConfigBytes. A non-empty configFile enables the
// file-migration pass; byte-only callers pass an empty path.
func loadConfigOptionalBytes(data []byte, optional bool, configFile string) (*Config, error) {
	var err error

	// In cloud deploy mode (optional=true), if file is empty or contains only whitespace, return empty config.
	if optional && len(bytes.TrimSpace(data)) == 0 {
		cfg := &Config{CredentialInFlight: DefaultCredentialInFlightConfig()}
		cfg.NormalizePluginsConfig()
		cfg.NormalizeClaudeConfig()
		return cfg, nil
	}

	if errValidate := validateCredentialWeightYAML(data); errValidate != nil {
		if optional {
			cfgOptional := &Config{CredentialInFlight: DefaultCredentialInFlightConfig()}
			cfgOptional.NormalizePluginsConfig()
			cfgOptional.NormalizeClaudeConfig()
			return cfgOptional, nil
		}
		return nil, errValidate
	}

	// Unmarshal the YAML data into the Config struct.
	var cfg Config
	// Set defaults before unmarshal so that absent keys keep defaults.
	cfg.Host = "" // Default empty: binds to all interfaces (IPv4 + IPv6)
	cfg.LoggingToFile = false
	cfg.LogsMaxTotalSizeMB = 0
	cfg.ErrorLogsMaxFiles = 10
	cfg.UsageStatisticsEnabled = false
	cfg.RedisUsageQueueRetentionSeconds = 60
	cfg.DisableCooling = false
	cfg.SaveCooldownStatus = false
	cfg.TransientErrorCooldownSeconds = 0
	cfg.DisableImageGeneration = DisableImageGenerationOff
	cfg.WebsocketAuth = true
	cfg.Pprof.Enable = false
	cfg.Pprof.Addr = DefaultPprofAddr
	cfg.Discovery.Enabled = false
	cfg.Discovery.ServiceType = DefaultDiscoveryServiceType
	cfg.Discovery.Subtypes = []string{"_chat-completions", "_responses", "_messages", "_generate-content", "_interactions"}
	cfg.RemoteManagement.PanelGitHubRepository = DefaultPanelGitHubRepository
	cfg.CredentialInFlight = DefaultCredentialInFlightConfig()
	if err = yaml.Unmarshal(data, &cfg); err != nil {
		if optional {
			// In cloud deploy mode, if YAML parsing fails, return empty config instead of error.
			cfgOptional := &Config{CredentialInFlight: DefaultCredentialInFlightConfig()}
			cfgOptional.NormalizePluginsConfig()
			cfgOptional.NormalizeClaudeConfig()
			return cfgOptional, nil
		}
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}
	if errValidate := validateTrustedProxies(cfg.TrustedProxies); errValidate != nil {
		return nil, errValidate
	}

	cfg.CredentialConcurrency = cfg.CredentialConcurrency.WithDefaults()
	if errValidate := cfg.CredentialInFlight.Validate(); errValidate != nil {
		return nil, errValidate
	}
	if cfg.Discovery.ServiceType == "" {
		cfg.Discovery.ServiceType = DefaultDiscoveryServiceType
	}
	if len(cfg.Discovery.Subtypes) == 0 {
		cfg.Discovery.Subtypes = []string{"_chat-completions", "_responses", "_messages", "_generate-content", "_interactions"}
	}
	if errValidate := cfg.Codex.LiveMediaRelay.Validate(); errValidate != nil {
		return nil, errValidate
	}
	if errValidate := cfg.ValidateCredentialWeights(); errValidate != nil {
		return nil, errValidate
	}

	// Hash remote management key if plaintext is detected (nested)
	// We consider a value to be already hashed if it looks like a bcrypt hash ($2a$, $2b$, or $2y$ prefix).
	var managementPlaintext, managementHashed string
	if cfg.RemoteManagement.SecretKey != "" && !looksLikeBcrypt(cfg.RemoteManagement.SecretKey) {
		managementPlaintext = cfg.RemoteManagement.SecretKey
		hashed, errHash := hashSecret(cfg.RemoteManagement.SecretKey)
		if errHash != nil {
			return nil, fmt.Errorf("failed to hash remote management key: %w", errHash)
		}
		cfg.RemoteManagement.SecretKey = hashed
		managementHashed = hashed
	}

	cfg.RemoteManagement.PanelGitHubRepository = strings.TrimSpace(cfg.RemoteManagement.PanelGitHubRepository)
	if cfg.RemoteManagement.PanelGitHubRepository == "" {
		cfg.RemoteManagement.PanelGitHubRepository = DefaultPanelGitHubRepository
	}

	cfg.Pprof.Addr = strings.TrimSpace(cfg.Pprof.Addr)
	if cfg.Pprof.Addr == "" {
		cfg.Pprof.Addr = DefaultPprofAddr
	}

	if cfg.LogsMaxTotalSizeMB < 0 {
		cfg.LogsMaxTotalSizeMB = 0
	}

	if cfg.ErrorLogsMaxFiles < 0 {
		cfg.ErrorLogsMaxFiles = 10
	}

	if cfg.RedisUsageQueueRetentionSeconds <= 0 {
		cfg.RedisUsageQueueRetentionSeconds = 60
	} else if cfg.RedisUsageQueueRetentionSeconds > 3600 {
		log.WithField("value", cfg.RedisUsageQueueRetentionSeconds).Warn("redis-usage-queue-retention-seconds too large; clamping to 3600")
		cfg.RedisUsageQueueRetentionSeconds = 3600
	}

	if cfg.MaxRetryCredentials < 0 {
		cfg.MaxRetryCredentials = 0
	}

	cfg.NormalizePluginsConfig()
	cfg.NormalizeClaudeConfig()
	if errResolvePluginsDir := cfg.ResolvePluginsDir(); errResolvePluginsDir != nil && cfg.Plugins.Enabled {
		return nil, errResolvePluginsDir
	}

	// Sanitize Gemini API key configuration and migrate legacy entries.
	cfg.SanitizeGeminiKeys()

	// Sanitize native Interactions API key configuration.
	cfg.SanitizeInteractionsKeys()

	// Sanitize Vertex-compatible API keys.
	cfg.SanitizeVertexCompatKeys()

	// Sanitize Codex keys: drop entries without base-url
	cfg.SanitizeCodexKeys()

	// Sanitize xAI keys: drop entries without base-url
	cfg.SanitizeXAIKeys()

	// Sanitize Meta keys.
	cfg.SanitizeMetaKeys()

	// Sanitize Codex header defaults.
	cfg.SanitizeCodexHeaderDefaults()

	// Sanitize Claude header defaults.
	cfg.SanitizeClaudeHeaderDefaults()

	// Sanitize Claude key headers
	cfg.SanitizeClaudeKeys()

	// Sanitize OpenAI compatibility providers: drop entries without base-url
	cfg.SanitizeOpenAICompatibility()

	// Normalize OAuth provider model exclusion map.
	cfg.OAuthExcludedModels = NormalizeOAuthExcludedModels(cfg.OAuthExcludedModels)

	// Normalize global OAuth model name aliases.
	cfg.SanitizeOAuthModelAlias()
	cfg.SanitizeKiroKeys()
	cfg.SanitizeBTKeys()

	// Normalize global OAuth model settings.
	cfg.SanitizeOAuthSettings()

	// Normalize global OAuth request-scoped error rules.
	cfg.SanitizeOAuthRequestScopedErrors()

	// Validate raw payload rules and drop invalid entries.
	cfg.SanitizePayloadRules()

	// Persist loader-derived on-disk state: a plaintext management secret is
	// replaced by its bcrypt hash and conflicting legacy fields are removed.
	// The update closure runs against the bytes read under the file lock, so a
	// concurrent in-process save is merged rather than overwritten by the
	// pre-lock snapshot this config was parsed from.
	if configFile != "" {
		legacyCleanupPending := false
		errPersist := UpdateConfigFile(configFile, func(current []byte) ([]byte, error) {
			updated := current
			secretPath := managementSecretKeyPath(updated)
			if secret, ok := nestedScalarValue(updated, strings.Join(secretPath, ".")); ok && secret != "" && !looksLikeBcrypt(secret) {
				// Reuse the hash the runtime config already holds when the file
				// still carries the same plaintext; hashing again would produce a
				// different bcrypt salt and leave the applied config and disk
				// disagreeing. A newer plaintext on disk is hashed as-is and is
				// never overwritten by the pre-lock snapshot hash.
				hashed := managementHashed
				if secret != managementPlaintext || hashed == "" {
					var errHash error
					hashed, errHash = hashSecret(secret)
					if errHash != nil {
						return nil, &configLoadPersistError{err: fmt.Errorf("failed to hash remote management key: %w", errHash)}
					}
				}
				next, errSet := updateNestedScalarYAML(updated, secretPath, hashed)
				if errSet != nil {
					return nil, &configLoadPersistError{err: fmt.Errorf("failed to persist hashed remote management key: %w", errSet)}
				}
				updated = next
			}
			// Only conflicting legacy fields are removed on load. A legacy-only
			// document stays legacy until a v8 configuration write migrates it.
			cleaned, changed, errLayout := NormalizeConfigLayout(updated, false)
			if errLayout != nil {
				return nil, &configLoadPersistError{err: errLayout}
			}
			if changed {
				updated = cleaned
				legacyCleanupPending = true
			}
			return updated, nil
		})
		var fatal *configLoadPersistError
		if errors.As(errPersist, &fatal) {
			return nil, fatal.err
		}
		if errPersist != nil {
			if legacyCleanupPending {
				// Removing conflicting legacy fields is part of the applied config,
				// so a failed cleanup write must abort instead of leaving the file
				// and the runtime shape disagreeing.
				return nil, fmt.Errorf("clean conflicting config fields: %w", errPersist)
			}
			// Persisting the hashed secret was best-effort before this change:
			// a read-only file still loads and re-hashes on the next start.
			log.Warnf("failed to persist hashed remote management key: %v", errPersist)
		}
	}

	// Record the exact bytes this config was parsed from, so a consumer can tell
	// it apart from a newer version of the file.
	cfg.SourceHash = ComputeSourceHash(data)

	// Return the populated configuration struct.
	return &cfg, nil
}

// configLoadPersistError marks a failure computing the loader migration, as
// opposed to a failure writing the migrated bytes.
type configLoadPersistError struct{ err error }

func (e *configLoadPersistError) Error() string { return e.err.Error() }
func (e *configLoadPersistError) Unwrap() error { return e.err }

// nestedScalarValue returns the scalar at dottedPath (e.g.
// "management.secret-key") after expanding YAML aliases.
func nestedScalarValue(data []byte, dottedPath string) (string, bool) {
	var root yaml.Node
	if errUnmarshal := yaml.Unmarshal(data, &root); errUnmarshal != nil {
		return "", false
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 || root.Content[0] == nil {
		return "", false
	}
	node := yamlPath(expandConfigAliases(root.Content[0]), dottedPath)
	if node == nil || node.Kind != yaml.ScalarNode || node.ShortTag() == "!!null" {
		return "", false
	}
	return node.Value, true
}
