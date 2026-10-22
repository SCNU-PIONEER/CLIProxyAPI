package config

import "fmt"

// ParseConfigBytes parses a YAML configuration payload into Config and applies the
// same in-memory normalization and validation pipeline as LoadConfigOptional,
// without reading or writing a file.
//
// It is retained as the strict entry point for management payloads: an empty
// payload is a request error rather than an all-defaults configuration. It
// shares the in-memory core with LoadConfigOptional but skips file migration.
func ParseConfigBytes(data []byte) (*Config, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("config payload is empty")
	}
	cfg, err := loadConfigOptionalBytes(data, false, "")
	if err != nil {
		return nil, err
	}
	// A management payload has no source file; keep ParseConfigBytes free of
	// file-snapshot bookkeeping so two equivalent payloads compare equal.
	cfg.SourceHash = ""
	return cfg, nil
}
