package store

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// TestObjectTokenStorePersistConfigUploadsCurrentConfig verifies that PersistConfig
// uploads exactly the bytes on disk after each save instead of a stale document.
func TestObjectTokenStorePersistConfigUploadsCurrentConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	objectStore, uploads := newSeedTestObjectStore(t, configPath)
	ctx := context.Background()

	writeSeedTestFile(t, configPath, "port: 8317\n")
	if errPersist := objectStore.PersistConfig(ctx); errPersist != nil {
		t.Fatalf("PersistConfig() error = %v", errPersist)
	}
	writeSeedTestFile(t, configPath, "port: 8318\n")
	if errPersist := objectStore.PersistConfig(ctx); errPersist != nil {
		t.Fatalf("PersistConfig() error = %v", errPersist)
	}

	if len(*uploads) != 2 {
		t.Fatalf("config uploads = %d, want 2", len(*uploads))
	}
	if got := string((*uploads)[0]); got != "port: 8317\n" {
		t.Fatalf("first upload = %q, want the first saved config", got)
	}
	if got := string((*uploads)[1]); got != "port: 8318\n" {
		t.Fatalf("second upload = %q, want the second saved config", got)
	}
}

// TestObjectTokenStorePersistConfigDoesNotUploadEmptyFile keeps the existing contract: an
// empty file deletes the remote config instead of replacing it with an empty document.
func TestObjectTokenStorePersistConfigDoesNotUploadEmptyFile(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	objectStore, uploads := newSeedTestObjectStore(t, configPath)
	writeSeedTestFile(t, configPath, "")

	if errPersist := objectStore.PersistConfig(context.Background()); errPersist != nil {
		t.Fatalf("PersistConfig() error = %v", errPersist)
	}
	if len(*uploads) != 0 {
		t.Fatalf("config uploads = %d, want an empty file to delete instead of upload", len(*uploads))
	}
}

// TestObjectTokenStorePersistConfigToleratesMissingFile keeps the existing contract: a
// missing file deletes the remote config and is not an error.
func TestObjectTokenStorePersistConfigToleratesMissingFile(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	objectStore, uploads := newSeedTestObjectStore(t, configPath)

	if errPersist := objectStore.PersistConfig(context.Background()); errPersist != nil {
		t.Fatalf("PersistConfig() error = %v", errPersist)
	}
	if len(*uploads) != 0 {
		t.Fatalf("config uploads = %d, want no upload for a missing file", len(*uploads))
	}
}

// TestPostgresStorePersistConfigUpsertsCurrentConfig verifies that PersistConfig upserts
// the normalized bytes on disk after each save.
func TestPostgresStorePersistConfigUpsertsCurrentConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	postgresStore, database := newSeedTestPostgresStore(t, configPath)
	ctx := context.Background()

	writeSeedTestFile(t, configPath, "port: 8317\r\n# note\r\n")
	if errPersist := postgresStore.PersistConfig(ctx); errPersist != nil {
		t.Fatalf("PersistConfig() error = %v", errPersist)
	}
	writeSeedTestFile(t, configPath, "port: 8318\n")
	if errPersist := postgresStore.PersistConfig(ctx); errPersist != nil {
		t.Fatalf("PersistConfig() error = %v", errPersist)
	}

	upserts := database.recordedUpserts()
	if len(upserts) != 2 {
		t.Fatalf("config upserts = %d, want 2", len(upserts))
	}
	if upserts[0] != "port: 8317\n# note\n" {
		t.Fatalf("first upsert = %q, want normalized first saved config", upserts[0])
	}
	if upserts[1] != "port: 8318\n" {
		t.Fatalf("second upsert = %q, want second saved config", upserts[1])
	}
}

// TestPostgresStorePersistConfigDeletesRecordForMissingFile keeps the existing contract: a
// missing file deletes the stored config and is not an error.
func TestPostgresStorePersistConfigDeletesRecordForMissingFile(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	postgresStore, database := newSeedTestPostgresStore(t, configPath)

	if errPersist := postgresStore.PersistConfig(context.Background()); errPersist != nil {
		t.Fatalf("PersistConfig() error = %v", errPersist)
	}
	if deletes := database.recordedDeletes(); deletes != 1 {
		t.Fatalf("config deletes = %d, want 1", deletes)
	}
	if upserts := database.recordedUpserts(); len(upserts) != 0 {
		t.Fatalf("config upserts = %d, want no upsert for a missing file", len(upserts))
	}
}

// configPersister is the persistence step shared by the stores under test.
type configPersister interface {
	PersistConfig(context.Context) error
}

// assertPersistWaitsForInPlaceWrite holds the config write lock through a callback and
// checks that PersistConfig neither completes nor persists anything until the write ends.
func assertPersistWaitsForInPlaceWrite(t *testing.T, configPath string, persister configPersister, persisted func() []string) {
	t.Helper()
	writeSeedTestFile(t, configPath, "port: 8317\n")

	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseWrite := func() {
		releaseOnce.Do(func() { close(release) })
	}
	defer releaseWrite()
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- config.UpdateConfigFile(configPath, func([]byte) ([]byte, error) {
			close(entered)
			<-release
			return []byte("port: 8318\n"), nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("config update did not reach the callback")
	}

	persistDone := make(chan error, 1)
	go func() {
		persistDone <- persister.PersistConfig(context.Background())
	}()

	// The bounded wait only gives an unserialized reader the chance to complete; a
	// reader that takes the shared lock must stay blocked until the write ends.
	select {
	case <-persistDone:
		t.Fatal("PersistConfig completed while the in-place config write was still running")
	case <-time.After(200 * time.Millisecond):
	}
	if got := persisted(); len(got) != 0 {
		t.Fatalf("persisted %d config(s) while the write lock was held", len(got))
	}

	releaseWrite()
	if errWrite := <-writeDone; errWrite != nil {
		t.Fatalf("UpdateConfigFile() error = %v", errWrite)
	}
	select {
	case errPersist := <-persistDone:
		if errPersist != nil {
			t.Fatalf("PersistConfig() error = %v", errPersist)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PersistConfig did not resume after the write completed")
	}

	got := persisted()
	if len(got) != 1 {
		t.Fatalf("persisted config count = %d, want 1", len(got))
	}
	if got[0] != "port: 8318\n" {
		t.Fatalf("persisted %q, want the completed write", got[0])
	}
}

func TestObjectTokenStorePersistConfigWaitsForInPlaceWrite(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	objectStore, uploads := newSeedTestObjectStore(t, configPath)
	assertPersistWaitsForInPlaceWrite(t, configPath, objectStore, func() []string {
		out := make([]string, 0, len(*uploads))
		for _, data := range *uploads {
			out = append(out, string(data))
		}
		return out
	})
}

func TestPostgresStorePersistConfigWaitsForInPlaceWrite(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	postgresStore, database := newSeedTestPostgresStore(t, configPath)
	assertPersistWaitsForInPlaceWrite(t, configPath, postgresStore, database.recordedUpserts)
}
