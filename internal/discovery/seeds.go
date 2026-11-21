package discovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Nosvemos/Darkorbit-Resource-Downloader/internal/manifest"
	"github.com/Nosvemos/Darkorbit-Resource-Downloader/internal/model"
)

const (
	userAgent        = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	probeConcurrency = 4
	maxDrainBytes    = 64 << 10
	maxTextBytes     = 4 << 20
)

var (
	langParamPattern    = regexp.MustCompile(`lang=([A-Za-z_-]+)`)
	languageCodePattern = regexp.MustCompile(`(?i)^[a-z]{2,3}(_[a-z]{2})?$`)
)

var hardcodedSeeds = []model.Seed{
	{RelativePath: "crossdomain.xml", Category: "core", Required: true},
	{RelativePath: "spacemap/preloader.swf", Category: "core", Required: true},
	{RelativePath: "spacemap/loadingscreen.swf", Category: "core", Required: true},
	{RelativePath: "spacemap/main.swf", Category: "core", Required: true},
	{RelativePath: "spacemap/xml/profile.xml", Category: "spacemap", Required: true},
	{RelativePath: "spacemap/xml/resources.xml", Category: "spacemap", Required: true},
	{RelativePath: "spacemap/xml/resources_3d.xml", Category: "spacemap", Required: true},
	{RelativePath: "spacemap/xml/resources_3d_particles.xml", Category: "spacemap", Required: true},
	{RelativePath: "spacemap/xml/assets_loadingScreen.xml", Category: "spacemap", Required: true},
	{RelativePath: "do_img/global/xml/resource_items.xml", Category: "do_img"},
	{RelativePath: "do_img/global/xml/resource_events.xml", Category: "do_img"},
	{RelativePath: "do_img/global/xml/resource_achievements.xml", Category: "do_img"},
	{RelativePath: "do_img/global/xml/resource_jumpgate.xml", Category: "do_img"},
	{RelativePath: "resources/resource_command_center.xml", Category: "resources"},
	{RelativePath: "resources/command-center/positions.xml", Category: "resources"},
}

var scanRoots = []string{
	"spacemap/xml",
	"spacemap/templates",
	"do_img",
	"unityApi",
	"flashAPI",
	"resources",
}

// DiscoverSeeds returns the hard-coded seeds plus every seed-like XML file
// already present under outputDir. Locally discovered seeds that are not in
// the hard-coded list are optional (Required == false). Paths are deduplicated
// case-insensitively.
func DiscoverSeeds(outputDir string) ([]model.Seed, error) {
	seen := map[string]model.Seed{}
	for _, seed := range hardcodedSeeds {
		seen[seedKey(seed.RelativePath)] = seed
	}

	for _, relRoot := range scanRoots {
		root := filepath.Join(outputDir, filepath.FromSlash(relRoot))
		err := filepath.WalkDir(root, func(current string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				if current == root {
					return nil
				}
				return walkErr
			}
			if d.IsDir() {
				if strings.Contains(filepath.ToSlash(current), "spacemap_decompiled") {
					return filepath.SkipDir
				}
				return nil
			}
			rel, err := filepath.Rel(outputDir, current)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if !isSeedCandidate(rel) {
				return nil
			}
			key := seedKey(rel)
			if _, ok := seen[key]; !ok {
				seen[key] = model.Seed{
					RelativePath: rel,
					Category:     categoryFor(rel),
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	return sortedSeeds(seen), nil
}

func seedKey(rel string) string {
	return strings.ToLower(toSlash(rel))
}

func sortedSeeds(seen map[string]model.Seed) []model.Seed {
	seeds := make([]model.Seed, 0, len(seen))
	for _, seed := range seen {
		seeds = append(seeds, seed)
	}
	sort.Slice(seeds, func(i, j int) bool {
		return seeds[i].RelativePath < seeds[j].RelativePath
	})
	return seeds
}

func categoryFor(rel string) string {
	switch {
	case rel == "crossdomain.xml" || strings.HasSuffix(rel, ".swf"):
		return "core"
	case strings.HasPrefix(rel, "spacemap/templates/"):
		return "templates"
	case strings.HasPrefix(rel, "spacemap/"):
		return "spacemap"
	case strings.HasPrefix(rel, "do_img/"):
		return "do_img"
	case strings.HasPrefix(rel, "unityApi/"):
		return "unityApi"
	case strings.HasPrefix(rel, "flashAPI/"):
		return "flashAPI"
	case strings.HasPrefix(rel, "resources/"):
		return "resources"
	default:
		return "other"
	}
}

func isSeedCandidate(rel string) bool {
	if strings.Contains(rel, "spacemap_decompiled") {
		return false
	}
	base := path.Base(rel)
	switch {
	case strings.HasPrefix(rel, "spacemap/templates/"):
		return strings.HasPrefix(base, "language_") && strings.HasSuffix(base, ".xml")
	case strings.HasPrefix(rel, "do_img/global/xml/") && strings.HasSuffix(rel, ".xml"):
		return true
	case strings.HasPrefix(rel, "do_img/") && strings.HasSuffix(rel, "/xml/resource_localized.xml"):
		return true
	case strings.HasPrefix(rel, "spacemap/xml/") && strings.HasSuffix(rel, ".xml"):
		return true
	case strings.HasPrefix(rel, "resources/") && strings.HasSuffix(rel, ".xml"):
		return true
	case strings.HasPrefix(rel, "unityApi/events/") && strings.HasSuffix(rel, ".xml"):
		return false
	default:
		return false
	}
}

// AddLanguageBootstrapSeeds adds the language manifests for the resolved
// languages. They are required seeds, as before.
func AddLanguageBootstrapSeeds(outputDir string, seeds []model.Seed, languages map[string]bool) []model.Seed {
	seen := make(map[string]model.Seed, len(seeds))
	for _, seed := range seeds {
		seen[seedKey(seed.RelativePath)] = seed
	}

	for _, lang := range resolveBootstrapLanguages(outputDir, seeds, languages) {
		for _, seed := range []model.Seed{
			{RelativePath: "spacemap/templates/language_" + lang + ".xml", Category: "templates", Required: true},
			{RelativePath: "do_img/" + lang + "/xml/resource_localized.xml", Category: "do_img", Required: true},
		} {
			seen[seedKey(seed.RelativePath)] = seed
		}
	}

	return sortedSeeds(seen)
}

func ResolveBootstrapLanguages(outputDir string, seeds []model.Seed, languages map[string]bool) []string {
	return resolveBootstrapLanguages(outputDir, seeds, languages)
}

func resolveBootstrapLanguages(outputDir string, seeds []model.Seed, languages map[string]bool) []string {
	if len(languages) == 0 {
		return []string{"en"}
	}

	langs := map[string]bool{}
	if languages["all"] {
		for _, lang := range discoverExistingLanguages(outputDir, seeds) {
			langs[lang] = true
		}
		if len(langs) == 0 {
			langs["en"] = true
		}
	} else {
		for lang := range languages {
			if lang == "" || lang == "all" {
				continue
			}
			langs[lang] = true
		}
	}

	resolved := make([]string, 0, len(langs))
	for lang := range langs {
		resolved = append(resolved, lang)
	}
	sort.Strings(resolved)
	return resolved
}

// LanguageProbeFailure records a language whose bootstrap files could not be
// probed (network error or an unexpected HTTP status).
type LanguageProbeFailure struct {
	Language string
	Err      error
}

// DiscoverLiveLanguages is DiscoverLiveLanguageReport without the
// per-language failure list.
func DiscoverLiveLanguages(ctx context.Context, baseURL string) ([]string, error) {
	languages, _, err := DiscoverLiveLanguageReport(ctx, baseURL)
	return languages, err
}

// DiscoverLiveLanguageReport reads the language links from the site's start
// page and keeps the languages whose bootstrap manifests exist. A language
// whose probes fail is reported in failures and skipped; the error is
// non-nil only when the start page cannot be fetched or every probe failed.
func DiscoverLiveLanguageReport(ctx context.Context, baseURL string) ([]string, []LanguageProbeFailure, error) {
	client := &http.Client{Timeout: 20 * time.Second}
	return discoverLiveLanguages(ctx, client, strings.TrimRight(baseURL, "/"))
}

// DiscoverAvailableSeeds probes the optional seeds and drops the ones the
// server reports missing (404, 410 or a redirect). Required seeds are kept
// without probing, and a probe that errors keeps its seed. The returned
// error is non-nil only when ctx is cancelled.
func DiscoverAvailableSeeds(ctx context.Context, baseURL string, seeds []model.Seed) ([]model.Seed, []string, error) {
	client := newProbeClient(15 * time.Second)
	baseURL = strings.TrimRight(baseURL, "/")

	exists := make([]bool, len(seeds))
	probeEach(len(seeds), func(i int) {
		if !IsOptionalSeed(seeds[i]) {
			exists[i] = true
			return
		}
		ok, err := urlExists(ctx, client, manifest.JoinURL(baseURL, seeds[i].RelativePath, ""))
		exists[i] = ok || err != nil
	})
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	kept := make([]model.Seed, 0, len(seeds))
	skipped := make([]string, 0)
	for i, seed := range seeds {
		if exists[i] {
			kept = append(kept, seed)
			continue
		}
		skipped = append(skipped, seed.RelativePath)
	}
	return kept, skipped, nil
}

func discoverLiveLanguages(ctx context.Context, client *http.Client, baseURL string) ([]string, []LanguageProbeFailure, error) {
	html, err := fetchText(ctx, client, baseURL)
	if err != nil {
		return nil, nil, err
	}

	probeClient := *client
	probeClient.CheckRedirect = noRedirect

	candidates := extractLanguagesFromHTML(html)
	exists := make([]bool, len(candidates))
	errs := make([]error, len(candidates))
	probeEach(len(candidates), func(i int) {
		exists[i], errs[i] = languageBootstrapExists(ctx, &probeClient, baseURL, candidates[i])
	})

	available := make([]string, 0, len(candidates))
	var failures []LanguageProbeFailure
	for i, lang := range candidates {
		switch {
		case exists[i]:
			available = append(available, lang)
		case errs[i] != nil:
			failures = append(failures, LanguageProbeFailure{Language: lang, Err: errs[i]})
		}
	}
	if len(candidates) > 0 && len(failures) == len(candidates) {
		joined := make([]error, len(failures))
		for i, failure := range failures {
			joined[i] = failure.Err
		}
		return nil, failures, fmt.Errorf("probing all %d live languages failed: %w", len(candidates), errors.Join(joined...))
	}
	return available, failures, nil
}

// probeEach calls fn(0..n-1) with at most probeConcurrency calls in flight.
// Callers write results into index-addressed slices so order stays stable.
func probeEach(n int, fn func(i int)) {
	sem := make(chan struct{}, probeConcurrency)
	var wg sync.WaitGroup
	for i := range n {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			fn(i)
		})
	}
	wg.Wait()
}

func discoverExistingLanguages(outputDir string, seeds []model.Seed) []string {
	langs := map[string]bool{}
	for _, seed := range seeds {
		if lang, ok := languageFromSeedPath(seed.RelativePath); ok {
			langs[CanonicalLanguageCode(lang)] = true
		}
	}

	patterns := []string{
		filepath.Join(outputDir, "spacemap", "templates", "language_*.xml"),
		filepath.Join(outputDir, "do_img", "*", "xml", "resource_localized.xml"),
	}
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, match := range matches {
			rel, err := filepath.Rel(outputDir, match)
			if err != nil {
				continue
			}
			if lang, ok := languageFromSeedPath(filepath.ToSlash(rel)); ok {
				langs[CanonicalLanguageCode(lang)] = true
			}
		}
	}

	resolved := make([]string, 0, len(langs))
	for lang := range langs {
		resolved = append(resolved, lang)
	}
	sort.Strings(resolved)
	return resolved
}

func languageFromSeedPath(rel string) (string, bool) {
	rel = toSlash(rel)

	if strings.HasPrefix(rel, "spacemap/templates/language_") && strings.HasSuffix(rel, ".xml") {
		lang := CanonicalLanguageCode(strings.TrimSuffix(strings.TrimPrefix(rel, "spacemap/templates/language_"), ".xml"))
		if lang != "" {
			return lang, true
		}
	}

	if strings.HasPrefix(rel, "do_img/") && strings.HasSuffix(rel, "/xml/resource_localized.xml") {
		parts := strings.Split(rel, "/")
		if len(parts) >= 4 && parts[1] != "" && parts[1] != "global" {
			if lang := CanonicalLanguageCode(parts[1]); lang != "" {
				return lang, true
			}
		}
	}

	return "", false
}

func extractLanguagesFromHTML(html string) []string {
	matches := langParamPattern.FindAllStringSubmatch(html, -1)
	seen := map[string]bool{}
	langs := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		lang := CanonicalLanguageCode(match[1])
		if lang == "" || lang == "all" || seen[lang] {
			continue
		}
		seen[lang] = true
		langs = append(langs, lang)
	}
	sort.Strings(langs)
	return langs
}

// languageBootstrapExists reports whether either bootstrap manifest of lang
// exists. An error is returned only when neither was found and at least one
// probe failed.
func languageBootstrapExists(ctx context.Context, client *http.Client, baseURL, lang string) (bool, error) {
	paths := []string{
		"spacemap/templates/language_" + lang + ".xml",
		"do_img/" + lang + "/xml/resource_localized.xml",
	}
	var firstErr error
	for _, rel := range paths {
		ok, err := urlExists(ctx, client, manifest.JoinURL(baseURL, rel, ""))
		if ok {
			return true, nil
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return false, firstErr
}

func newProbeClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: noRedirect}
}

// noRedirect makes the client return 3xx responses as-is: for an existence
// probe a redirect (often to a landing page) means the file is not there.
func noRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

func newRequest(ctx context.Context, method, target string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "*/*")
	return req, nil
}

func drainAndClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes))
	_ = resp.Body.Close()
}

// urlExists probes target with HEAD, falling back to a one-byte ranged GET
// when the server does not allow HEAD. 2xx (and 416 on the ranged GET) mean
// the file exists; 404, 410 and 3xx mean it does not; anything else is an
// error.
func urlExists(ctx context.Context, client *http.Client, target string) (bool, error) {
	req, err := newRequest(ctx, http.MethodHead, target)
	if err != nil {
		return false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	if resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusNotImplemented {
		drainAndClose(resp)
		req, err = newRequest(ctx, http.MethodGet, target)
		if err != nil {
			return false, err
		}
		req.Header.Set("Range", "bytes=0-0")
		resp, err = client.Do(req)
		if err != nil {
			return false, err
		}
		if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
			drainAndClose(resp)
			return true, nil
		}
	}
	defer drainAndClose(resp)

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return true, nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return false, nil
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return false, nil
	default:
		return false, &httpStatusError{statusCode: resp.StatusCode, target: target}
	}
}

func fetchText(ctx context.Context, client *http.Client, target string) (string, error) {
	req, err := newRequest(ctx, http.MethodGet, target)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer drainAndClose(resp)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &httpStatusError{statusCode: resp.StatusCode, target: target}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxTextBytes))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

type httpStatusError struct {
	statusCode int
	target     string
}

func (e *httpStatusError) Error() string {
	return "unexpected status " + strconv.Itoa(e.statusCode) + " for " + e.target
}

// CanonicalLanguageCode normalizes a language code to "xx" or "xx_YY" form
// ("-" becomes "_"). It returns "" for codes containing anything other than
// ASCII letters, "_" and "-", or with empty parts.
func CanonicalLanguageCode(code string) string {
	code = strings.TrimSpace(code)
	if code == "" {
		return ""
	}
	for _, r := range code {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && r != '_' && r != '-' {
			return ""
		}
	}
	if strings.EqualFold(code, "all") {
		return "all"
	}

	code = strings.ReplaceAll(code, "-", "_")
	parts := strings.Split(code, "_")
	for i := range parts {
		if parts[i] == "" {
			return ""
		}
		if i == 0 {
			parts[i] = strings.ToLower(parts[i])
			continue
		}
		parts[i] = strings.ToUpper(parts[i])
	}
	return strings.Join(parts, "_")
}

// IsOptionalSeed reports whether a missing seed may be skipped instead of
// failing the sync. It is the inverse of seed.Required.
func IsOptionalSeed(seed model.Seed) bool {
	return !seed.Required
}

func FilterSeeds(seeds []model.Seed, allowed map[string]bool, includeCore bool) []model.Seed {
	if len(allowed) == 0 {
		return seeds
	}
	filtered := make([]model.Seed, 0, len(seeds))
	for _, seed := range seeds {
		if seed.Category == "core" && !includeCore {
			continue
		}
		if allowed[seed.Category] || allowed["all"] || (seed.Category == "core" && allowed["core"]) {
			filtered = append(filtered, seed)
		}
	}
	return filtered
}

// MatchesLanguagePath reports whether rel belongs to one of the selected
// languages. Only path segments that look like a language code (or are one
// of the selected codes) are treated as languages; every other path is
// included.
func MatchesLanguagePath(rel string, languages map[string]bool) bool {
	if len(languages) == 0 || languages["all"] {
		return true
	}

	rel = toSlash(rel)
	parts := strings.Split(rel, "/")

	if strings.HasPrefix(rel, "spacemap/templates/language_") && strings.HasSuffix(rel, ".xml") && len(parts) == 3 {
		return matchesLanguageSegment(strings.TrimSuffix(strings.TrimPrefix(rel, "spacemap/templates/language_"), ".xml"), languages)
	}

	// spacemap/templates/<lang>/...; a file directly under templates is not
	// language-specific.
	if strings.HasPrefix(rel, "spacemap/templates/") && len(parts) >= 4 {
		return matchesLanguageSegment(parts[2], languages)
	}

	// do_img/<lang>/...
	if strings.HasPrefix(rel, "do_img/") && len(parts) >= 3 {
		return matchesLanguageSegment(parts[1], languages)
	}

	return true
}

// matchesLanguageSegment is true when segment is a selected language or does
// not look like a language code at all.
func matchesLanguageSegment(segment string, languages map[string]bool) bool {
	code := CanonicalLanguageCode(segment)
	if code == "" {
		return true
	}
	if languages[code] {
		return true
	}
	return !languageCodePattern.MatchString(code)
}

// toSlash normalizes server-style paths the same way on every OS.
func toSlash(rel string) string {
	return strings.ReplaceAll(rel, "\\", "/")
}
