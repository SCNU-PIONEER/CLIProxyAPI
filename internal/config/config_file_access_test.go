package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestUpdateConfigFilePreservesInodeAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(path, []byte("port: 8317\n"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	before, errStat := os.Stat(path)
	if errStat != nil {
		t.Fatal(errStat)
	}

	if errUpdate := UpdateConfigFile(path, func(data []byte) ([]byte, error) {
		return append(data, []byte("request-retry: 2\n")...), nil
	}); errUpdate != nil {
		t.Fatal(errUpdate)
	}

	after, errStat := os.Stat(path)
	if errStat != nil {
		t.Fatal(errStat)
	}
	if !os.SameFile(before, after) {
		t.Fatal("in-place update replaced the file instead of keeping its inode")
	}
	if runtime.GOOS != "windows" && before.Mode().Perm() != after.Mode().Perm() {
		t.Fatalf("permissions changed from %v to %v", before.Mode().Perm(), after.Mode().Perm())
	}
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if want := "port: 8317\nrequest-retry: 2\n"; string(data) != want {
		t.Fatalf("content = %q, want %q", string(data), want)
	}
}

func TestUpdateConfigFileKeepsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "real.yaml")
	link := filepath.Join(dir, "config.yaml")
	if errWrite := os.WriteFile(target, []byte("port: 8317\n"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errLink := os.Symlink(target, link); errLink != nil {
		t.Fatal(errLink)
	}

	if errUpdate := UpdateConfigFile(link, func([]byte) ([]byte, error) {
		return []byte("port: 9000\n"), nil
	}); errUpdate != nil {
		t.Fatal(errUpdate)
	}

	info, errLstat := os.Lstat(link)
	if errLstat != nil {
		t.Fatal(errLstat)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("in-place update replaced the symlink")
	}
	data, errRead := os.ReadFile(target)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if got, want := string(data), "port: 9000\n"; got != want {
		t.Fatalf("symlink target content = %q, want %q", got, want)
	}
}

func TestWriteConfigFileCreatesMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := WriteConfigFile(path, []byte("port: 8317\n")); errWrite != nil {
		t.Fatal(errWrite)
	}
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if got, want := string(data), "port: 8317\n"; got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
}

func TestUpdateConfigFileDoesNotWriteOnCallbackError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(path, []byte("port: 8317\n"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	sentinel := os.ErrInvalid
	if errUpdate := UpdateConfigFile(path, func([]byte) ([]byte, error) {
		return nil, sentinel
	}); errUpdate == nil {
		t.Fatal("expected the callback error to be returned")
	}
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if got, want := string(data), "port: 8317\n"; got != want {
		t.Fatalf("content after failed update = %q, want %q", got, want)
	}
}

func TestReadConfigFileWaitsForInPlaceWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(path, []byte("port: 8317\n"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- UpdateConfigFile(path, func(data []byte) ([]byte, error) {
			close(entered)
			<-release
			return append(data, []byte("request-retry: 2\n")...), nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("update did not reach the callback")
	}

	readDone := make(chan struct{})
	go func() {
		_, _ = ReadConfigFile(path)
		close(readDone)
	}()
	// The bounded wait only gives an unserialized reader the chance to complete;
	// a reader that takes the shared lock must stay blocked until the write ends.
	select {
	case <-readDone:
		t.Fatal("reader completed while the in-place write was still running")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if errWrite := <-writeDone; errWrite != nil {
		t.Fatal(errWrite)
	}
	select {
	case <-readDone:
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not resume after the write completed")
	}
}

func TestLoadConfigOptionalSettledFileDoesNotRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	hashed, errHash := hashSecret("s3cret")
	if errHash != nil {
		t.Fatal(errHash)
	}
	raw := []byte("port: 8317\nremote-management:\n  secret-key: \"" + hashed + "\"\n")
	if errWrite := os.WriteFile(path, raw, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	settled := time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC)
	if errChtimes := os.Chtimes(path, settled, settled); errChtimes != nil {
		t.Fatal(errChtimes)
	}

	cfg, errLoad := LoadConfigOptional(path, false)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	if cfg.RemoteManagement.SecretKey != hashed {
		t.Fatalf("secret key = %q, want the settled hash %q", cfg.RemoteManagement.SecretKey, hashed)
	}

	info, errStat := os.Stat(path)
	if errStat != nil {
		t.Fatal(errStat)
	}
	if !info.ModTime().Equal(settled) {
		t.Fatalf("settled config was rewritten: mtime = %v, want %v", info.ModTime(), settled)
	}
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if string(data) != string(raw) {
		t.Fatalf("settled config content changed:\n got %q\nwant %q", string(data), string(raw))
	}
}

func TestLoadConfigOptionalHashesSecretAndRemovesConflictingLegacyFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := []byte("debug: true\nobservability:\n  logs:\n    debug: false\nremote-management:\n  secret-key: \"plaintext-secret\"\n")
	if errWrite := os.WriteFile(path, raw, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}

	if _, errLoad := LoadConfigOptional(path, false); errLoad != nil {
		t.Fatal(errLoad)
	}

	data, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if strings.Contains(string(data), "plaintext-secret") {
		t.Fatalf("plaintext secret was left on disk: %s", string(data))
	}
	if !strings.Contains(string(data), "$2") {
		t.Fatalf("hashed secret was not persisted: %s", string(data))
	}
	var doc map[string]any
	if errUnmarshal := yaml.Unmarshal(data, &doc); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if _, exists := doc["debug"]; exists {
		t.Fatalf("conflicting legacy debug field was not removed: %s", string(data))
	}
}

func TestLoadConfigOptionalPreservesConcurrentSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := []byte("port: 8317\nremote-management:\n  secret-key: \"plaintext-secret\"\n")
	if errWrite := os.WriteFile(path, raw, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- UpdateConfigFile(path, func(data []byte) ([]byte, error) {
			close(entered)
			<-release
			return append(data, []byte("request-retry: 7\n")...), nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent save did not start")
	}

	loadDone := make(chan error, 1)
	go func() {
		_, errLoad := LoadConfigOptional(path, false)
		loadDone <- errLoad
	}()
	// The loader must wait for the in-flight save instead of reading the file
	// and later writing a snapshot that drops the save.
	select {
	case errLoad := <-loadDone:
		t.Fatalf("load finished while the save held the lock: %v", errLoad)
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	if errWrite := <-writeDone; errWrite != nil {
		t.Fatal(errWrite)
	}
	if errLoad := <-loadDone; errLoad != nil {
		t.Fatal(errLoad)
	}

	data, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if !strings.Contains(string(data), "request-retry: 7") {
		t.Fatalf("concurrent save was lost: %s", string(data))
	}
	if strings.Contains(string(data), "plaintext-secret") || !strings.Contains(string(data), "$2") {
		t.Fatalf("secret was not hashed from fresh bytes: %s", string(data))
	}
}

func TestUpdateConfigFileWritesWhenCallbackMutatesInputInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(path, []byte("port: 8317\n"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}

	// fn is allowed to transform the bytes it receives in place and return the
	// same slice; that still changes the file and must be written. Comparing the
	// post-callback slice against itself would wrongly classify it as unchanged.
	if errUpdate := UpdateConfigFile(path, func(data []byte) ([]byte, error) {
		data[0] = 'X'
		return data, nil
	}); errUpdate != nil {
		t.Fatal(errUpdate)
	}

	data, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if len(data) == 0 || data[0] != 'X' {
		t.Fatalf("in-place callback mutation was not written: %q", string(data))
	}
}

func TestLoadConfigOptionalStillDefaultsEmptyFileAtStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(path, nil, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	cfg, errLoad := LoadConfigOptional(path, false)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	if !cfg.WebsocketAuth {
		t.Fatal("startup load of an empty file must keep the websocket-auth default")
	}
	if cfg.Pprof.Addr != DefaultPprofAddr {
		t.Fatalf("pprof addr = %q, want default %q", cfg.Pprof.Addr, DefaultPprofAddr)
	}
}
