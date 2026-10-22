package executor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestPersistRefreshedAuthSkipsConfigManagedAuth(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "config-kiro.json")
	auth := &cliproxyauth.Auth{
		ID:       "kiro-config-1",
		Provider: "kiro",
		Attributes: map[string]string{
			"source": "config:kiro[abc123]",
			"path":   authPath,
		},
		Metadata: map[string]any{"type": "kiro", "access_token": "token"},
	}

	if err := (&KiroExecutor{cfg: &config.Config{AuthDir: dir}}).persistRefreshedAuth(auth); err != nil {
		t.Fatalf("persist config-managed auth: %v", err)
	}
	if _, err := os.Stat(authPath); !os.IsNotExist(err) {
		t.Fatalf("config-managed auth was written to %s (err=%v); canonical manager must own persistence", authPath, err)
	}
	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		t.Fatalf("read temp dir: %v", errRead)
	}
	if len(entries) != 0 {
		t.Fatalf("config-managed persist left files behind: %v", entries)
	}
}

func TestPersistRefreshedAuthWritesAtomically(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	auth := &cliproxyauth.Auth{
		ID:         "kiro-file-1",
		Provider:   "kiro",
		Attributes: map[string]string{"path": authPath},
		Metadata:   map[string]any{"type": "kiro", "access_token": "new-token"},
	}

	if err := (&KiroExecutor{cfg: &config.Config{AuthDir: dir}}).persistRefreshedAuth(auth); err != nil {
		t.Fatalf("persist refreshed auth: %v", err)
	}
	raw, errRead := os.ReadFile(authPath)
	if errRead != nil {
		t.Fatalf("read persisted auth: %v", errRead)
	}
	var metadata map[string]any
	if errUnmarshal := json.Unmarshal(raw, &metadata); errUnmarshal != nil {
		t.Fatalf("decode persisted auth: %v", errUnmarshal)
	}
	if got := metadata["access_token"]; got != "new-token" {
		t.Fatalf("access_token = %v, want new-token", got)
	}
	entries, errDir := os.ReadDir(dir)
	if errDir != nil {
		t.Fatalf("read temp dir: %v", errDir)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Fatalf("temporary file left behind: %s", entry.Name())
		}
	}
}

func TestPersistRefreshedAuthReplacesSymlinkWithoutFollowingIt(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "target.json")
	const targetContent = `{"keep":"original"}`
	if errWrite := os.WriteFile(targetPath, []byte(targetContent), 0o600); errWrite != nil {
		t.Fatalf("write symlink target: %v", errWrite)
	}
	linkPath := filepath.Join(dir, "linked.json")
	if errLink := os.Symlink(targetPath, linkPath); errLink != nil {
		t.Skipf("symlinks unavailable on this host: %v", errLink)
	}
	auth := &cliproxyauth.Auth{
		ID:         "kiro-file-2",
		Provider:   "kiro",
		Attributes: map[string]string{"path": linkPath},
		Metadata:   map[string]any{"type": "kiro", "access_token": "new-token"},
	}

	if err := (&KiroExecutor{cfg: &config.Config{AuthDir: dir}}).persistRefreshedAuth(auth); err != nil {
		t.Fatalf("persist refreshed auth over symlink: %v", err)
	}
	kept, errRead := os.ReadFile(targetPath)
	if errRead != nil {
		t.Fatalf("read symlink target: %v", errRead)
	}
	if string(kept) != targetContent {
		t.Fatalf("symlink target was modified: %s", kept)
	}
	info, errStat := os.Lstat(linkPath)
	if errStat != nil {
		t.Fatalf("stat auth path: %v", errStat)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("auth path is still a symlink; expected it to be replaced")
	}
	published, errPublished := os.ReadFile(linkPath)
	if errPublished != nil {
		t.Fatalf("read published auth: %v", errPublished)
	}
	if !strings.Contains(string(published), "new-token") {
		t.Fatalf("published auth missing refreshed metadata: %s", published)
	}
}
