package config

import (
	"bytes"
	"fmt"
	"os"
	"sync"
)

// configFileMu serializes configuration file reads and writes performed through
// the participating helpers in this file (ReadConfigFile, UpdateConfigFile and
// WriteConfigFile).
//
// A management save writes with the truncate flag, so a reader that opens the
// file during that window can observe an empty or partial document. Holding the
// write lock for the whole write and the read lock for the whole read keeps a
// reload from observing such an intermediate state produced through these
// helpers. Writes that bypass them (for example Git repository sync/staging) and
// writers outside this process (editors, scripts, other processes) remain
// uncoordinated and can still expose partial documents.
var configFileMu sync.RWMutex

// ReadConfigFile returns the current bytes of path while holding the shared
// lock. The lock is released before the caller parses the returned bytes, so
// parsing never happens under the lock.
func ReadConfigFile(path string) ([]byte, error) {
	configFileMu.RLock()
	defer configFileMu.RUnlock()
	return os.ReadFile(path)
}

// UpdateConfigFile applies fn to the current bytes of path and writes the
// result back in place. The file must already exist. Writing in place keeps the
// inode, the symlink target and the existing permissions unchanged (callers
// rely on os.SameFile and on mounted-config inode preservation).
//
// fn runs while the write lock is held, so it must only transform bytes: it
// must not call ReadConfigFile, UpdateConfigFile or a configuration loader that
// may call them, because the lock is not reentrant. fn may mutate the provided
// slice in place and return it.
func UpdateConfigFile(path string, fn func([]byte) ([]byte, error)) error {
	if fn == nil {
		return fmt.Errorf("config: nil config file update function")
	}
	configFileMu.Lock()
	defer configFileMu.Unlock()

	data, errRead := os.ReadFile(path)
	if errRead != nil {
		return errRead
	}
	// Keep a pristine copy of the on-disk bytes: fn is allowed to mutate its
	// input in place, so comparing against data after the call could mistake an
	// in-place update for an unchanged document.
	original := bytes.Clone(data)
	updated, errUpdate := fn(data)
	if errUpdate != nil {
		return errUpdate
	}
	// Migration passes are idempotent: a file that already has the desired
	// shape must not be rewritten. Apart from avoiding pointless I/O, this keeps
	// read-only settled config files loadable, which the previous
	// read-normalize-then-write-only-when-changed loader relied on.
	if bytes.Equal(original, updated) {
		return nil
	}
	return os.WriteFile(path, updated, 0o600)
}

// WriteConfigFile writes data to path in place while holding the exclusive
// lock. It preserves the management write contract: the file is created if it
// is missing, truncated, synced and closed.
func WriteConfigFile(path string, data []byte) error {
	configFileMu.Lock()
	defer configFileMu.Unlock()

	file, errOpen := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if errOpen != nil {
		return errOpen
	}
	if _, errWrite := file.Write(data); errWrite != nil {
		_ = file.Close()
		return errWrite
	}
	if errSync := file.Sync(); errSync != nil {
		_ = file.Close()
		return errSync
	}
	return file.Close()
}
