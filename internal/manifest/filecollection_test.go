package manifest_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Nosvemos/Darkorbit-Resource-Downloader/internal/manifest"
)

const xmlHeader = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n"

func writeManifest(t *testing.T, name, content string) string {
	t.Helper()
	filePath := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return filePath
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name         string
		manifestPath string
		body         string
		wantPaths    []string
		wantURLs     []string
		wantSkipped  int
	}{
		{
			name:         "location node",
			manifestPath: "spacemap/xml/resources.xml",
			body: `<filecollection>
  <location id="graphics" path="graphics/" />
  <file id="demo" location="graphics" name="portal" type="swf" hash="abc12300" version="1" />
</filecollection>`,
			wantPaths: []string{"spacemap/graphics/portal.swf"},
			wantURLs:  []string{"https://example.com/spacemap/graphics/portal.swf?__cv=abc12300"},
		},
		{
			name:         "language collection without location nodes",
			manifestPath: "spacemap/templates/language_en.xml",
			body: `<filecollection>
  <file id="resource_chat" location="en" name="resource_chat" type="xml" hash="abc12300" version="1" />
</filecollection>`,
			wantPaths: []string{"spacemap/templates/en/resource_chat.xml"},
			wantURLs:  []string{"https://example.com/spacemap/templates/en/resource_chat.xml?__cv=abc12300"},
		},
		{
			name:         "declared location with empty path is not replaced by its id",
			manifestPath: "spacemap/xml/resources.xml",
			body: `<filecollection>
  <location id="root" path="" />
  <file id="a" location="root" name="a" type="swf" />
</filecollection>`,
			wantPaths: []string{"spacemap/a.swf"},
			wantURLs:  []string{"https://example.com/spacemap/a.swf"},
		},
		{
			name:         "location without trailing slash",
			manifestPath: "spacemap/xml/resources.xml",
			body: `<filecollection>
  <location id="g" path="graphics/ships" />
  <file id="a" location="g" name="a" type="png" />
</filecollection>`,
			wantPaths: []string{"spacemap/graphics/ships/a.png"},
		},
		{
			name:         "backslash location is normalized",
			manifestPath: "spacemap/xml/resources.xml",
			body: `<filecollection>
  <location id="g" path="graphics\ships" />
  <file id="a" location="g" name="a" type="png" />
</filecollection>`,
			wantPaths: []string{"spacemap/graphics/ships/a.png"},
		},
		{
			name:         "empty name is skipped",
			manifestPath: "spacemap/xml/resources.xml",
			body: `<filecollection>
  <file id="a" location="g" name="" type="png" />
  <file id="b" location="g" name="b" type="png" />
</filecollection>`,
			wantPaths:   []string{"spacemap/g/b.png"},
			wantSkipped: 1,
		},
		{
			name:         "empty type has no extension",
			manifestPath: "spacemap/xml/resources.xml",
			body: `<filecollection>
  <file id="a" location="g" name="LICENSE" type="" />
</filecollection>`,
			wantPaths: []string{"spacemap/g/LICENSE"},
		},
		{
			name:         "special characters are escaped in the url",
			manifestPath: "spacemap/xml/resources.xml",
			body: `<filecollection>
  <location id="g" path="my dir/" />
  <file id="a" location="g" name="a#b?c%d" type="png" hash="x y&amp;z" />
</filecollection>`,
			wantPaths: []string{"spacemap/my dir/a#b?c%d.png"},
			wantURLs:  []string{"https://example.com/spacemap/my%20dir/a%23b%3Fc%25d.png?__cv=x+y%26z"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filePath := writeManifest(t, "manifest.xml", xmlHeader+tt.body)
			result, ok, err := manifest.Load(filePath, tt.manifestPath, "https://example.com/", "spacemap")
			if err != nil {
				t.Fatalf("Load error: %v", err)
			}
			if !ok {
				t.Fatal("expected a filecollection")
			}
			if len(result.Resources) != len(tt.wantPaths) {
				t.Fatalf("got %d resources, want %d: %+v", len(result.Resources), len(tt.wantPaths), result.Resources)
			}
			for i, want := range tt.wantPaths {
				if got := result.Resources[i].RelativePath; got != want {
					t.Errorf("resource %d path = %q, want %q", i, got, want)
				}
				if got := result.Resources[i].ManifestPath; got != tt.manifestPath {
					t.Errorf("resource %d manifest path = %q, want %q", i, got, tt.manifestPath)
				}
			}
			for i, want := range tt.wantURLs {
				if got := result.Resources[i].URL; got != want {
					t.Errorf("resource %d url = %q, want %q", i, got, want)
				}
			}
			if len(result.Skipped) != tt.wantSkipped {
				t.Errorf("skipped = %+v, want %d entries", result.Skipped, tt.wantSkipped)
			}
		})
	}
}

func TestLoadRejectsUnsafePaths(t *testing.T) {
	tests := []struct {
		name         string
		manifestPath string
		location     string
		fileName     string
	}{
		{name: "dot-dot in location", manifestPath: "spacemap/xml/resources.xml", location: "../../../", fileName: "evil"},
		{name: "dot-dot in name", manifestPath: "spacemap/xml/resources.xml", location: "g/", fileName: "../../../../evil"},
		{name: "backslash traversal", manifestPath: "spacemap/xml/resources.xml", location: `..\..\..\`, fileName: "evil"},
		{name: "absolute location", manifestPath: "manifest.xml", location: "/etc/", fileName: "evil"},
		{name: "drive letter", manifestPath: "manifest.xml", location: `C:\Windows\`, fileName: "evil"},
		{name: "colon in name", manifestPath: "spacemap/xml/resources.xml", location: "g/", fileName: "evil:stream"},
		{name: "resolves to base dir", manifestPath: "manifest.xml", location: "", fileName: "."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := xmlHeader + `<filecollection>
  <location id="l" path="` + tt.location + `" />
  <file id="bad" location="l" name="` + tt.fileName + `" type="" />
  <file id="good" location="ok" name="good" type="xml" />
</filecollection>`
			filePath := writeManifest(t, "manifest.xml", body)
			result, ok, err := manifest.Load(filePath, tt.manifestPath, "https://example.com", "spacemap")
			if err != nil {
				t.Fatalf("Load error: %v", err)
			}
			if !ok {
				t.Fatal("expected a filecollection")
			}
			if len(result.Skipped) != 1 || result.Skipped[0].ID != "bad" {
				t.Errorf("skipped = %+v, want only the bad entry", result.Skipped)
			}
			if len(result.Resources) != 1 || !strings.HasSuffix(result.Resources[0].RelativePath, "ok/good.xml") {
				t.Errorf("resources = %+v, want only the good entry", result.Resources)
			}
			for _, resource := range result.Resources {
				if strings.Contains(resource.RelativePath, "..") || strings.ContainsAny(resource.RelativePath, `\:`) {
					t.Errorf("unsafe path leaked: %q", resource.RelativePath)
				}
			}
		})
	}
}

func TestLoadDocumentKinds(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantOK  bool
		wantErr bool
	}{
		{name: "truncated filecollection", content: xmlHeader + `<filecollection><file id="a" name="a"`, wantErr: true},
		{name: "unclosed root", content: xmlHeader + `<filecollection><file id="a" name="a" type="xml"/>`, wantErr: true},
		{name: "html error page", content: "<!DOCTYPE html>\n<html><body>502 Bad Gateway</body></html>"},
		{name: "other xml root", content: xmlHeader + `<profile><item/></profile>`},
		{name: "binary data", content: "FWS\x0a\x00\x01\x02"},
		{name: "empty file", content: ""},
		{name: "unsupported charset", content: `<?xml version="1.0" encoding="SHIFT_JIS"?><filecollection/>`, wantErr: true},
		{name: "latin-1 charset", content: `<?xml version="1.0" encoding="ISO-8859-1"?><filecollection><file id="a" location="g" name="caf` + "\xe9" + `" type="xml"/></filecollection>`, wantOK: true},
		{name: "empty filecollection", content: xmlHeader + `<filecollection/>`, wantOK: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filePath := writeManifest(t, "manifest.xml", tt.content)
			result, ok, err := manifest.Load(filePath, "spacemap/xml/resources.xml", "https://example.com", "spacemap")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "spacemap/xml/resources.xml") {
				t.Errorf("error %q does not name the manifest", err)
			}
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
			if tt.name == "latin-1 charset" && (len(result.Resources) != 1 || result.Resources[0].RelativePath != "spacemap/g/café.xml") {
				t.Errorf("latin-1 resources = %+v", result.Resources)
			}
		})
	}
}

func TestLoadResourcesFromFileMissingFile(t *testing.T) {
	_, _, err := manifest.LoadResourcesFromFile(filepath.Join(t.TempDir(), "nope.xml"), "nope.xml", "https://example.com", "x")
	if !os.IsNotExist(err) {
		t.Errorf("err = %v, want a not-exist error", err)
	}
}

func TestJoinURL(t *testing.T) {
	tests := []struct {
		base, rel, hash, want string
	}{
		{"https://example.com", "spacemap/xml/resources.xml", "", "https://example.com/spacemap/xml/resources.xml"},
		{"https://example.com/", "/spacemap/main.swf", "abc", "https://example.com/spacemap/main.swf?__cv=abc"},
		{"https://example.com", `spacemap\xml\a.xml`, "", "https://example.com/spacemap/xml/a.xml"},
		{"https://example.com", "a b/c#d.png", "", "https://example.com/a%20b/c%23d.png"},
		{"https://example.com", "q?x=1/100%.png", "h#1", "https://example.com/q%3Fx=1/100%25.png?__cv=h%231"},
	}

	for _, tt := range tests {
		t.Run(tt.rel, func(t *testing.T) {
			if got := manifest.JoinURL(tt.base, tt.rel, tt.hash); got != tt.want {
				t.Errorf("JoinURL(%q, %q, %q) = %q, want %q", tt.base, tt.rel, tt.hash, got, tt.want)
			}
		})
	}
}
