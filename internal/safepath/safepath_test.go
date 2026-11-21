package safepath_test

import (
	"errors"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Nosvemos/Darkorbit-Resource-Downloader/internal/safepath"
)

func TestJoinAccepts(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		rel  string
		want string
	}{
		{"file.bin", filepath.Join(root, "file.bin")},
		{"spacemap/xml/resources.xml", filepath.Join(root, "spacemap", "xml", "resources.xml")},
		{"a/./b.txt", filepath.Join(root, "a", "b.txt")},
		{"a/../b.txt", filepath.Join(root, "b.txt")},
		{"dir/", filepath.Join(root, "dir")},
	}
	for _, tt := range tests {
		t.Run(tt.rel, func(t *testing.T) {
			got, err := safepath.Join(root, tt.rel)
			if err != nil {
				t.Fatalf("Join(%q) unexpected error: %v", tt.rel, err)
			}
			if got != tt.want {
				t.Fatalf("Join(%q) = %q, want %q", tt.rel, got, tt.want)
			}
		})
	}
}

func TestJoinRejects(t *testing.T) {
	root := t.TempDir()
	tests := []string{
		"",
		".",
		"a/..",
		"..",
		"../evil.bin",
		"a/../../evil.bin",
		"/etc/passwd",
		"//server/share/x",
		`a\b.txt`,
		`..\evil.bin`,
		`C:\Windows\evil.dll`,
		"C:/Windows/evil.dll",
		"C:evil.bin",
		"file.txt:stream",
	}
	if runtime.GOOS == "windows" {
		tests = append(tests,
			"NUL",
			"con",
			"dir/COM1",
			"LPT1",
			"CONIN$",
			"dir/aux",
		)
	}
	for _, rel := range tests {
		t.Run(rel, func(t *testing.T) {
			got, err := safepath.Join(root, rel)
			if err == nil {
				t.Fatalf("Join(%q) = %q, expected error", rel, got)
			}
			if !errors.Is(err, safepath.ErrUnsafePath) {
				t.Fatalf("Join(%q) error %v does not wrap ErrUnsafePath", rel, err)
			}
		})
	}
}
