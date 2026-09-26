package estimator

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
)

// FileStore persists state as an atomically replaced JSON file.
type FileStore struct {
	path string
}

func NewFileStore(path string) *FileStore {
	return &FileStore{path: path}
}

// Load reads the last committed state. A missing file means a clean start and
// yields an empty, non-error state.
func (f *FileStore) Load() (PersistedState, error) {
	var state PersistedState
	file, err := os.Open(f.path)
	if err != nil {
		if os.IsNotExist(err) {
			return state, nil
		}
		return state, err
	}
	defer file.Close()
	dec := json.NewDecoder(file)
	if err := dec.Decode(&state); err != nil {
		if err == io.EOF {
			return PersistedState{}, nil
		}
		return PersistedState{}, err
	}
	return state, nil
}

// Save writes state to a temp file in the same directory, fsyncs it, then
// renames it into place so readers never observe a torn file.
func (f *FileStore) Save(state PersistedState) error {
	if err := ensureDir(f.path); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(f.path, data)
}

var _ Store = (*FileStore)(nil)

func ensureDir(path string) error {
	return os.MkdirAll(filepath.Dir(path), 0o755)
}

func writeFileAtomic(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
