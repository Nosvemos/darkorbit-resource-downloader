package state_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Nosvemos/Darkorbit-Resource-Downloader/internal/state"
)

func TestLoadMissingReturnsEmpty(t *testing.T) {
	st, err := state.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if st == nil || st.Resources == nil || len(st.Resources) != 0 {
		t.Fatalf("expected empty state, got %+v", st)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := &state.State{Resources: map[string]state.StateEntry{
		"a/b.png": {Hash: "abc00", URL: "https://example.invalid/a/b.png"},
	}}
	if err := state.Save(dir, want); err != nil {
		t.Fatal(err)
	}
	// Overwrite an existing file to exercise rename-over-existing.
	want.Resources["c.swf"] = state.StateEntry{Hash: "def00"}
	if err := state.Save(dir, want); err != nil {
		t.Fatal(err)
	}

	got, err := state.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.UpdatedAt == "" {
		t.Fatal("expected UpdatedAt to be set")
	}
	if len(got.Resources) != 2 || got.Resources["a/b.png"] != want.Resources["a/b.png"] || got.Resources["c.swf"].Hash != "def00" {
		t.Fatalf("unexpected resources: %+v", got.Resources)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != state.FileName {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected only %s in dir, got %v", state.FileName, names)
	}
}

func TestSaveNilResources(t *testing.T) {
	dir := t.TempDir()
	if err := state.Save(dir, &state.State{}); err != nil {
		t.Fatal(err)
	}
	st, err := state.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Resources == nil {
		t.Fatal("expected non-nil resources")
	}
}

func TestLoadCorruptReturnsEmptyWithWarning(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, state.FileName), []byte(`{"resources": {"x": `), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := state.Load(dir)
	if !errors.Is(err, state.ErrCorrupt) {
		t.Fatalf("expected ErrCorrupt, got %v", err)
	}
	if st == nil || st.Resources == nil || len(st.Resources) != 0 {
		t.Fatalf("expected usable empty state, got %+v", st)
	}

	// The tool must recover: saving replaces the corrupt file.
	st.Resources["x"] = state.StateEntry{Hash: "h"}
	if err := state.Save(dir, st); err != nil {
		t.Fatal(err)
	}
	reloaded, err := state.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Resources["x"].Hash != "h" {
		t.Fatalf("unexpected reloaded state: %+v", reloaded)
	}
}

func TestLoadUnreadableIsHardError(t *testing.T) {
	dir := t.TempDir()
	// A directory where the state file should be cannot be read as a file.
	if err := os.Mkdir(filepath.Join(dir, state.FileName), 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := state.Load(dir)
	if err == nil || errors.Is(err, state.ErrCorrupt) {
		t.Fatalf("expected a non-corrupt hard error, got %v", err)
	}
	if st != nil {
		t.Fatalf("expected nil state on hard error, got %+v", st)
	}
}
