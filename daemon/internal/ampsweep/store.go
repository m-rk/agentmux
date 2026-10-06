package ampsweep

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// sweptDirName is the state dir under the run user's home holding one
// JSON record per swept thread, mirroring retire's retired records.
const sweptDirName = ".local/state/agentmux/swept"

// FileStore persists swept records as one JSON file per thread.
type FileStore struct {
	Home string
}

func (s FileStore) dir() string {
	return filepath.Join(s.Home, sweptDirName)
}

func (s FileStore) path(thread string) string {
	return filepath.Join(s.dir(), thread+".json")
}

// Save records one swept thread atomically (temp file and rename, 0600
// in a 0700 directory), like the amp thread-runner cache.
func (s FileStore) Save(rec SweptRecord) error {
	if s.Home == "" {
		return fmt.Errorf("no home directory for swept record")
	}
	if err := os.MkdirAll(s.dir(), 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", s.dir(), err)
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir(), ".swept-*")
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil || cerr != nil || os.Chmod(f.Name(), 0o600) != nil || os.Rename(f.Name(), s.path(rec.Thread)) != nil {
		os.Remove(f.Name())
		return fmt.Errorf("writing swept record for %s", rec.Thread)
	}
	return nil
}

// Load returns every swept record. A missing dir is nil, not an error;
// an unreadable record is skipped, never fatal — a corrupt record must
// not block the gc from collecting the rest.
func (s FileStore) Load() ([]SweptRecord, error) {
	entries, err := os.ReadDir(s.dir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []SweptRecord
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir(), e.Name()))
		if err != nil {
			continue
		}
		var rec SweptRecord
		if err := json.Unmarshal(data, &rec); err != nil || rec.Thread == "" {
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}

// Remove drops one swept record; a missing record is not an error.
func (s FileStore) Remove(thread string) error {
	if err := os.Remove(s.path(thread)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
