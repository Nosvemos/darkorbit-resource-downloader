package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const FileName = ".app-state.json"

// ErrCorrupt is returned (wrapped) by Load when the state file exists but
// cannot be parsed. Load still returns a usable empty state alongside it, so
// callers should treat errors.Is(err, ErrCorrupt) as a warning ("state was
// reset") and continue; the next Save overwrites the broken file.
var ErrCorrupt = errors.New("state file is corrupt")

type State struct {
	UpdatedAt string                `json:"updated_at"`
	Resources map[string]StateEntry `json:"resources"`
}

// StateEntry records what was last verified for one resource. Size and
// ModTime (Unix nanoseconds) describe the local file at that moment; when both
// still match and the manifest hash is unchanged, the file need not be
// re-hashed. Category is the --category the resource belongs to, so a filtered
// sync can tell which entries its selection covers. Entries written by older
// versions simply lack these fields.
type StateEntry struct {
	Hash     string `json:"hash,omitempty"`
	URL      string `json:"url,omitempty"`
	Category string `json:"category,omitempty"`
	Size     int64  `json:"size,omitempty"`
	ModTime  int64  `json:"mtime,omitempty"`
}

// Load reads the state file from outputDir. A missing file yields an empty
// state and a nil error. An unparsable file yields an empty state and an error
// wrapping ErrCorrupt. Any other read error yields a nil state.
func Load(outputDir string) (*State, error) {
	filePath := filepath.Join(outputDir, FileName)
	data, err := os.ReadFile(filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return newState(), nil
		}
		return nil, err
	}

	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return newState(), fmt.Errorf("%w: %s: %w", ErrCorrupt, filePath, err)
	}
	if st.Resources == nil {
		st.Resources = map[string]StateEntry{}
	}
	return &st, nil
}

// Save writes the state atomically: it writes a temporary file in outputDir,
// syncs it, and renames it over the state file, so a crash never leaves a
// truncated state file behind.
func Save(outputDir string, st *State) error {
	if st.Resources == nil {
		st.Resources = map[string]StateEntry{}
	}
	st.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(outputDir, FileName+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, filepath.Join(outputDir, FileName)); err != nil {
		return err
	}
	committed = true
	return nil
}

func newState() *State {
	return &State{Resources: map[string]StateEntry{}}
}
