package threadwatch

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// offsetsFileName is the file FileOffsets persists to inside its directory.
const offsetsFileName = "offsets.json"

// FileOffsets is an OffsetStore backed by a single offsets.json file in a
// state directory. It is safe for concurrent use.
type FileOffsets struct {
	mu      sync.Mutex
	path    string
	offsets map[string]int64
}

// LoadOffsets reads offsets.json from dir. A missing file is not an error:
// it returns an empty FileOffsets that will create the file on Save.
func LoadOffsets(dir string) (*FileOffsets, error) {
	path := filepath.Join(dir, offsetsFileName)
	fo := &FileOffsets{path: path, offsets: map[string]int64{}}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fo, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if len(data) == 0 {
		return fo, nil
	}
	if err := json.Unmarshal(data, &fo.offsets); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if fo.offsets == nil {
		fo.offsets = map[string]int64{}
	}
	return fo, nil
}

// Get returns the persisted offset for key, if any.
func (f *FileOffsets) Get(key string) (int64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.offsets[key]
	return v, ok
}

// Set records the offset for key. Callers must call Save to persist it.
func (f *FileOffsets) Set(key string, value int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.offsets[key] = value
}

// Save writes the offsets atomically (temp file + rename) as 0600, creating
// the parent directory (0700) if needed.
func (f *FileOffsets) Save() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	dir := filepath.Dir(f.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(f.offsets, "", "  ")
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".offsets-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, f.path)
}
