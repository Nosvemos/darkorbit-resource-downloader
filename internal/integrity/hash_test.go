package integrity_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Nosvemos/Darkorbit-Resource-Downloader/internal/integrity"
)

// md5("hello") = 5d41402abc4b2a76b9719d911017c592
const (
	helloFull       = "5d41402abc4b2a76b9719d911017c592"
	helloNormalized = "5d41402abc4b2a76b9719d911017c500"
)

func writeSample(t *testing.T) string {
	t.Helper()
	filePath := filepath.Join(t.TempDir(), "sample.bin")
	if err := os.WriteFile(filePath, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	return filePath
}

func TestNormalizedFileMD5(t *testing.T) {
	filePath := writeSample(t)

	hash, err := integrity.NormalizedFileMD5(filePath)
	if err != nil {
		t.Fatal(err)
	}
	if hash != helloNormalized {
		t.Fatalf("unexpected normalized hash: got %s want %s", hash, helloNormalized)
	}

	full, err := integrity.FileMD5(filePath)
	if err != nil {
		t.Fatal(err)
	}
	if full != helloFull {
		t.Fatalf("unexpected full hash: got %s want %s", full, helloFull)
	}
}

func TestFileMatchesManifestHash(t *testing.T) {
	filePath := writeSample(t)

	tests := []struct {
		name     string
		manifest string
		want     bool
	}{
		{"normalized match", helloNormalized, true},
		{"normalized uppercase", strings.ToUpper(helloNormalized), true},
		{"full match", helloFull, true},
		{"full uppercase", strings.ToUpper(helloFull), true},
		{"normalized mismatch", "00000000000000000000000000000000", false},
		{"full mismatch", "5d41402abc4b2a76b9719d911017c593", false},
		{"empty hash", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := integrity.FileMatchesManifestHash(filePath, tt.manifest)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("FileMatchesManifestHash(%q) = %v, want %v", tt.manifest, got, tt.want)
			}
		})
	}
}

func TestFileMatchesManifestHashMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.bin")
	match, err := integrity.FileMatchesManifestHash(missing, helloNormalized)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected not-exist error, got %v", err)
	}
	if match {
		t.Fatal("missing file must not match")
	}
}

func TestMatchesManifestHash(t *testing.T) {
	tests := []struct {
		name     string
		md5Hex   string
		manifest string
		want     bool
	}{
		{"normalized", helloFull, helloNormalized, true},
		{"full", helloFull, helloFull, true},
		{"mixed case digest", strings.ToUpper(helloFull), helloNormalized, true},
		// A manifest hash not ending in 00 must be compared in full, so a
		// digest differing only in the last byte must not match.
		{"full differs in last byte", helloFull, helloFull[:30] + "aa", false},
		{"empty manifest", helloFull, "", false},
		{"empty digest", "", helloNormalized, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := integrity.MatchesManifestHash(tt.md5Hex, tt.manifest); got != tt.want {
				t.Fatalf("MatchesManifestHash(%q, %q) = %v, want %v", tt.md5Hex, tt.manifest, got, tt.want)
			}
		})
	}
}
