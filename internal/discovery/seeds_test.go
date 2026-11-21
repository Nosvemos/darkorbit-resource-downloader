package discovery_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Nosvemos/Darkorbit-Resource-Downloader/internal/discovery"
	"github.com/Nosvemos/Darkorbit-Resource-Downloader/internal/model"
)

func writeFiles(t *testing.T, root string, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func seedsByPath(seeds []model.Seed) map[string]model.Seed {
	byPath := make(map[string]model.Seed, len(seeds))
	for _, seed := range seeds {
		byPath[seed.RelativePath] = seed
	}
	return byPath
}

func TestDiscoverSeeds(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root,
		"spacemap/templates/en/resource_chat.xml",
		"spacemap/xml/resources.xml",
		"spacemap/xml/custom_extra.xml",
		"do_img/global/xml/resource_items.xml",
		"resources/resource_command_center.xml",
		"unityApi/events/spaceSearch.php",
		"spacemap_decompiled/ignore.xml",
		"index.php",
	)

	seeds, err := discovery.DiscoverSeeds(root)
	if err != nil {
		t.Fatal(err)
	}
	seen := seedsByPath(seeds)

	tests := []struct {
		path         string
		wantPresent  bool
		wantCategory string
		wantRequired bool
	}{
		{path: "spacemap/templates/en/resource_chat.xml"},
		{path: "spacemap/xml/resources.xml", wantPresent: true, wantCategory: "spacemap", wantRequired: true},
		{path: "spacemap/xml/custom_extra.xml", wantPresent: true, wantCategory: "spacemap"},
		{path: "spacemap/main.swf", wantPresent: true, wantCategory: "core", wantRequired: true},
		{path: "do_img/global/xml/resource_items.xml", wantPresent: true, wantCategory: "do_img"},
		{path: "resources/resource_command_center.xml", wantPresent: true, wantCategory: "resources"},
		{path: "spacemap_decompiled/ignore.xml"},
		{path: "unityApi/events/spaceSearch.php"},
		{path: "index.php"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			seed, ok := seen[tt.path]
			if ok != tt.wantPresent {
				t.Fatalf("present = %v, want %v", ok, tt.wantPresent)
			}
			if !ok {
				return
			}
			if seed.Category != tt.wantCategory {
				t.Errorf("category = %q, want %q", seed.Category, tt.wantCategory)
			}
			if seed.Required != tt.wantRequired {
				t.Errorf("required = %v, want %v", seed.Required, tt.wantRequired)
			}
			if discovery.IsOptionalSeed(seed) == seed.Required {
				t.Errorf("IsOptionalSeed disagrees with Required=%v", seed.Required)
			}
		})
	}
}

func TestDiscoverSeedsDedupesCaseInsensitively(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, "spacemap/templates/language_EN.xml", "spacemap/XML/Resources.xml")

	seeds, err := discovery.DiscoverSeeds(root)
	if err != nil {
		t.Fatal(err)
	}
	seeds = discovery.AddLanguageBootstrapSeeds(root, seeds, map[string]bool{"en": true})

	counts := map[string]int{}
	for _, seed := range seeds {
		counts[filepath.ToSlash(seed.RelativePath)]++
	}
	lowered := map[string]int{}
	for rel, n := range counts {
		lowered[strings.ToLower(rel)] += n
	}
	for _, rel := range []string{"spacemap/templates/language_en.xml", "spacemap/xml/resources.xml"} {
		if lowered[rel] != 1 {
			t.Errorf("%s listed %d times: %v", rel, lowered[rel], counts)
		}
	}
	if counts["spacemap/templates/language_en.xml"] != 1 {
		t.Errorf("expected the canonical bootstrap path to win, got %v", counts)
	}
}

func TestAddLanguageBootstrapSeeds(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, "spacemap/templates/language_ja.xml", "do_img/fr/xml/resource_localized.xml")

	tests := []struct {
		name      string
		root      string
		languages map[string]bool
		want      map[string]string
	}{
		{
			name:      "arbitrary language codes",
			root:      t.TempDir(),
			languages: map[string]bool{"de": true, "au": true},
			want: map[string]string{
				"spacemap/templates/language_de.xml":   "templates",
				"spacemap/templates/language_au.xml":   "templates",
				"do_img/de/xml/resource_localized.xml": "do_img",
				"do_img/au/xml/resource_localized.xml": "do_img",
			},
		},
		{
			name:      "all uses discovered languages",
			root:      root,
			languages: map[string]bool{"all": true},
			want: map[string]string{
				"spacemap/templates/language_ja.xml":   "templates",
				"spacemap/templates/language_fr.xml":   "templates",
				"do_img/ja/xml/resource_localized.xml": "do_img",
				"do_img/fr/xml/resource_localized.xml": "do_img",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seen := seedsByPath(discovery.AddLanguageBootstrapSeeds(tt.root, nil, tt.languages))
			for rel, category := range tt.want {
				if seen[rel].Category != category {
					t.Errorf("%s category = %q, want %q", rel, seen[rel].Category, category)
				}
				if !seen[rel].Required {
					t.Errorf("%s should be required", rel)
				}
			}
		})
	}
}

func TestMatchesLanguagePath(t *testing.T) {
	tests := []struct {
		path      string
		languages map[string]bool
		want      bool
	}{
		{path: "spacemap/templates/language_en.xml", want: true},
		{path: "spacemap/templates/language_tr.xml", want: false},
		{path: "spacemap/templates/en/resource_chat.xml", want: true},
		{path: "spacemap/templates/es/resource_chat.xml", want: false},
		{path: "spacemap/templates/pt-BR/resource_chat.xml", want: false},
		{path: "spacemap/templates/EN/resource_chat.xml", want: true},
		{path: `spacemap\templates\es\resource_chat.xml`, want: false},
		{path: "spacemap/templates/shared/fonts.swf", want: true},
		{path: "spacemap/templates/settings.xml", want: true},
		{path: "spacemap/templates/es", want: true},
		{path: "do_img/en/xml/resource_localized.xml", want: true},
		{path: "do_img/es/xml/resource_localized.xml", want: false},
		{path: "do_img/global/xml/resource_items.xml", want: true},
		{path: "do_img/common_assets/a.png", want: true},
		{path: "do_img/readme.txt", want: true},
		{path: "spacemap/xml/resources.xml", want: true},
		{path: "spacemap/templates/de_DE/x.xml", languages: map[string]bool{"de_DE": true}, want: true},
		{path: "spacemap/templates/longcode/x.xml", languages: map[string]bool{"longcode": true}, want: true},
		{path: "spacemap/templates/tr/resource_chat.xml", languages: map[string]bool{"all": true}, want: true},
		{path: "do_img/es/xml/resource_localized.xml", languages: map[string]bool{"all": true}, want: true},
		{path: "do_img/es/xml/resource_localized.xml", languages: map[string]bool{}, want: true},
	}

	for _, tt := range tests {
		languages := tt.languages
		if languages == nil {
			languages = map[string]bool{"en": true}
		}
		t.Run(tt.path, func(t *testing.T) {
			if got := discovery.MatchesLanguagePath(tt.path, languages); got != tt.want {
				t.Errorf("MatchesLanguagePath(%q, %v) = %v, want %v", tt.path, languages, got, tt.want)
			}
		})
	}
}

func TestCanonicalLanguageCode(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"en", "en"},
		{"PT_br", "pt_BR"},
		{"es-ar", "es_AR"},
		{"en_US", "en_US"},
		{"ALL", "all"},
		{"  tr  ", "tr"},
		{"", ""},
		{"en_", ""},
		{"../en", ""},
		{"en/x", ""},
		{"e1", ""},
		{"é", ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := discovery.CanonicalLanguageCode(tt.in); got != tt.want {
				t.Errorf("CanonicalLanguageCode(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// fileServer answers HEAD/GET for the given paths with the given status
// (200 when zero) and 404 for everything else.
func fileServer(t *testing.T, statuses map[string]int, handler func(w http.ResponseWriter, r *http.Request) bool) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if handler != nil && handler(w, r) {
			return
		}
		status, ok := statuses[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte("<filecollection/>"))
		}
	}))
	t.Cleanup(server.Close)
	return server
}

const languageIndex = `
	<a href="index.es?lang=en">EN</a>
	<a href="index.es?lang=pt-BR">PTBR</a>
	<a href="index.es?lang=de">DE</a>
	<a href="index.es?lang=au">AU</a>
	<a href="index.es?lang=fr">FR</a>
`

func serveIndex(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/" {
		return false
	}
	_, _ = w.Write([]byte(languageIndex))
	return true
}

func TestDiscoverLiveLanguageReport(t *testing.T) {
	tests := []struct {
		name         string
		statuses     map[string]int
		handler      func(w http.ResponseWriter, r *http.Request) bool
		want         []string
		wantFailures []string
		wantErr      bool
	}{
		{
			name: "found via either bootstrap file",
			statuses: map[string]int{
				"/spacemap/templates/language_en.xml":    0,
				"/spacemap/templates/language_pt_BR.xml": 0,
				"/do_img/de/xml/resource_localized.xml":  0,
			},
			want: []string{"de", "en", "pt_BR"},
		},
		{
			name: "410 is treated as missing",
			statuses: map[string]int{
				"/spacemap/templates/language_en.xml":   0,
				"/spacemap/templates/language_de.xml":   http.StatusGone,
				"/do_img/de/xml/resource_localized.xml": http.StatusGone,
			},
			want: []string{"en"},
		},
		{
			name: "partial failure keeps the other languages",
			statuses: map[string]int{
				"/spacemap/templates/language_en.xml":   0,
				"/spacemap/templates/language_de.xml":   http.StatusForbidden,
				"/do_img/de/xml/resource_localized.xml": http.StatusTooManyRequests,
				"/spacemap/templates/language_fr.xml":   http.StatusInternalServerError,
				"/do_img/fr/xml/resource_localized.xml": 0,
			},
			want:         []string{"en", "fr"},
			wantFailures: []string{"de"},
		},
		{
			name: "every probe failing is an error",
			handler: func(w http.ResponseWriter, r *http.Request) bool {
				if serveIndex(w, r) {
					return true
				}
				w.WriteHeader(http.StatusServiceUnavailable)
				return true
			},
			wantFailures: []string{"au", "de", "en", "fr", "pt_BR"},
			wantErr:      true,
		},
		{
			name: "redirect is treated as missing",
			handler: func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path == "/spacemap/templates/language_de.xml" {
					http.Redirect(w, r, "/spacemap/templates/language_en.xml", http.StatusFound)
					return true
				}
				return false
			},
			statuses: map[string]int{"/spacemap/templates/language_en.xml": 0},
			want:     []string{"en"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := tt.handler
			if handler == nil {
				handler = serveIndex
			} else if tt.statuses != nil {
				inner := handler
				handler = func(w http.ResponseWriter, r *http.Request) bool {
					return serveIndex(w, r) || inner(w, r)
				}
			}
			server := fileServer(t, tt.statuses, handler)

			got, failures, err := discovery.DiscoverLiveLanguageReport(context.Background(), server.URL)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("languages = %v, want %v", got, tt.want)
			}
			var failed []string
			for _, failure := range failures {
				if failure.Err == nil {
					t.Errorf("failure for %s has nil error", failure.Language)
				}
				failed = append(failed, failure.Language)
			}
			if !slices.Equal(failed, tt.wantFailures) {
				t.Errorf("failures = %v, want %v", failed, tt.wantFailures)
			}
		})
	}
}

func TestDiscoverLiveLanguagesProbesWithHEAD(t *testing.T) {
	var mu sync.Mutex
	methods := map[string]bool{}
	server := fileServer(t, map[string]int{"/spacemap/templates/language_en.xml": 0}, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/" {
			mu.Lock()
			methods[r.Method] = true
			mu.Unlock()
		}
		return serveIndex(w, r)
	})

	if _, err := discovery.DiscoverLiveLanguages(context.Background(), server.URL); err != nil {
		t.Fatal(err)
	}
	if !methods[http.MethodHead] || methods[http.MethodGet] {
		t.Errorf("probe methods = %v, want HEAD only", methods)
	}
}

func TestDiscoverAvailableSeeds(t *testing.T) {
	headNotAllowed := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return true
		}
		if r.Header.Get("Range") != "bytes=0-0" {
			t.Errorf("GET fallback for %s without Range header", r.URL.Path)
		}
		return false
	}

	tests := []struct {
		name        string
		statuses    map[string]int
		handler     func(w http.ResponseWriter, r *http.Request) bool
		seeds       []model.Seed
		wantKept    []string
		wantSkipped []string
	}{
		{
			name: "skips optional 404 seeds",
			statuses: map[string]int{
				"/do_img/global/xml/resource_items.xml": 0,
			},
			seeds: []model.Seed{
				{RelativePath: "spacemap/xml/resources.xml", Category: "spacemap", Required: true},
				{RelativePath: "do_img/global/xml/resource_items.xml", Category: "do_img"},
				{RelativePath: "do_img/global/xml/resource_events.xml", Category: "do_img"},
			},
			wantKept:    []string{"spacemap/xml/resources.xml", "do_img/global/xml/resource_items.xml"},
			wantSkipped: []string{"do_img/global/xml/resource_events.xml"},
		},
		{
			name:     "HEAD not allowed falls back to ranged GET",
			statuses: map[string]int{"/a.xml": http.StatusPartialContent, "/c.xml": http.StatusGone},
			handler:  headNotAllowed,
			seeds: []model.Seed{
				{RelativePath: "a.xml"},
				{RelativePath: "b.xml"},
				{RelativePath: "c.xml"},
			},
			wantKept:    []string{"a.xml"},
			wantSkipped: []string{"b.xml", "c.xml"},
		},
		{
			name: "redirect is treated as missing",
			handler: func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path == "/moved.xml" {
					http.Redirect(w, r, "/", http.StatusMovedPermanently)
					return true
				}
				w.WriteHeader(http.StatusOK)
				return true
			},
			seeds:       []model.Seed{{RelativePath: "moved.xml"}, {RelativePath: "here.xml"}},
			wantKept:    []string{"here.xml"},
			wantSkipped: []string{"moved.xml"},
		},
		{
			name:     "probe errors keep the seed",
			statuses: map[string]int{"/flaky.xml": http.StatusInternalServerError},
			seeds:    []model.Seed{{RelativePath: "flaky.xml"}},
			wantKept: []string{"flaky.xml"},
		},
		{
			name:     "locally discovered seed is optional and order is preserved",
			statuses: map[string]int{"/z.xml": 0, "/a.xml": 0, "/m.xml": 0},
			seeds: []model.Seed{
				{RelativePath: "z.xml"}, {RelativePath: "gone1.xml"}, {RelativePath: "a.xml"},
				{RelativePath: "gone2.xml"}, {RelativePath: "m.xml"}, {RelativePath: "gone3.xml"},
			},
			wantKept:    []string{"z.xml", "a.xml", "m.xml"},
			wantSkipped: []string{"gone1.xml", "gone2.xml", "gone3.xml"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := fileServer(t, tt.statuses, tt.handler)
			kept, skipped, err := discovery.DiscoverAvailableSeeds(context.Background(), server.URL, tt.seeds)
			if err != nil {
				t.Fatal(err)
			}
			var keptPaths []string
			for _, seed := range kept {
				keptPaths = append(keptPaths, seed.RelativePath)
			}
			if !slices.Equal(keptPaths, tt.wantKept) {
				t.Errorf("kept = %v, want %v", keptPaths, tt.wantKept)
			}
			if !slices.Equal(skipped, tt.wantSkipped) {
				t.Errorf("skipped = %v, want %v", skipped, tt.wantSkipped)
			}
		})
	}
}

func TestDiscoverAvailableSeedsLocalSeedIsOptional(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, "spacemap/xml/local_only.xml")
	seeds, err := discovery.DiscoverSeeds(root)
	if err != nil {
		t.Fatal(err)
	}

	server := fileServer(t, nil, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/spacemap/xml/local_only.xml" {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		w.WriteHeader(http.StatusOK)
		return true
	})
	_, skipped, err := discovery.DiscoverAvailableSeeds(context.Background(), server.URL, seeds)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(skipped, "spacemap/xml/local_only.xml") {
		t.Errorf("skipped = %v, want local_only.xml skipped", skipped)
	}
	if slices.Contains(skipped, "spacemap/xml/resources.xml") {
		t.Errorf("required hard-coded seed was probed and skipped: %v", skipped)
	}
}

func TestDiscoverAvailableSeedsCancelled(t *testing.T) {
	server := fileServer(t, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := discovery.DiscoverAvailableSeeds(ctx, server.URL, []model.Seed{{RelativePath: "a.xml"}}); err == nil {
		t.Error("expected an error for a cancelled context")
	}
}
