package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Nosvemos/Darkorbit-Resource-Downloader/internal/discovery"
	"github.com/Nosvemos/Darkorbit-Resource-Downloader/internal/downloader"
	"github.com/Nosvemos/Darkorbit-Resource-Downloader/internal/integrity"
	"github.com/Nosvemos/Darkorbit-Resource-Downloader/internal/manifest"
	"github.com/Nosvemos/Darkorbit-Resource-Downloader/internal/model"
	"github.com/Nosvemos/Darkorbit-Resource-Downloader/internal/safepath"
	"github.com/Nosvemos/Darkorbit-Resource-Downloader/internal/state"
)

func runSync(ctx context.Context, cfg config, pr *printer) error {
	if removed, err := downloader.RemoveStaleParts(cfg.OutputDir); err != nil {
		pr.Warnf("removing leftover .part files: %v", err)
	} else if removed > 0 {
		pr.Status("CLEAN", fmt.Sprintf("removed %d leftover .part files", removed))
	}

	dl := newDownloader(cfg)
	cfg, seeds, err := prepareSeeds(ctx, cfg, pr, true)
	if err != nil {
		return err
	}
	seeds, fetched, err := fetchSeeds(ctx, dl, cfg, seeds, true, pr)
	if err != nil {
		return err
	}

	resources, manifestsParsed, _ := loadResources(cfg, seeds, pr)
	plan, skipped, st, err := BuildDownloadPlan(ctx, cfg.OutputDir, resources, stateScope(cfg), cfg.Force, pr.Warnf)
	if err != nil {
		return err
	}

	printSummary("sync", model.Summary{
		SeedsFetched:     fetched,
		ManifestCount:    manifestsParsed,
		ResourceCount:    len(resources),
		PlannedDownloads: len(plan),
		SkippedResources: skipped,
	}, pr)
	printCategoryBreakdownResources(resources, "Resources by category", pr)
	printCategoryBreakdownPlan(plan, "Planned downloads by category", pr)
	printReasonBreakdown(plan, pr)

	if len(plan) == 0 {
		pr.Println("No resource downloads needed.")
		return state.Save(cfg.OutputDir, st)
	}

	results := dl.DownloadAll(ctx, cfg.OutputDir, plan, cfg.Concurrency)
	var completed, failed, mismatched, interrupted int
	total := len(plan)
	lastEffective := -1
	for result := range results {
		rel := result.Item.Resource.RelativePath
		if result.Err != nil && errors.Is(result.Err, context.Canceled) {
			interrupted++
			continue
		}
		stats := dl.Stats()
		if stats.AutoTuneEnabled && stats.EffectiveConcurrency != lastEffective {
			pr.Status("TUNE", fmt.Sprintf("effective concurrency %d/%d (min %d, in-flight %d)", stats.EffectiveConcurrency, stats.MaxConcurrency, stats.MinConcurrency, stats.InFlight))
			lastEffective = stats.EffectiveConcurrency
		}
		live := fmt.Sprintf("limit %d/%d, in-flight %d", stats.EffectiveConcurrency, stats.MaxConcurrency, stats.InFlight)
		done := completed + failed + 1
		if result.Err != nil {
			failed++
			var hashErr *downloader.HashMismatchError
			if errors.As(result.Err, &hashErr) {
				mismatched++
				pr.Status("HASH", fmt.Sprintf("[%d/%d] %s: downloaded content does not match manifest hash (expected %s, got %s)", done, total, rel, hashErr.Expected, hashErr.Actual))
				continue
			}
			pr.Status("FAIL", fmt.Sprintf("[%d/%d] %s (%s, %s): %v", done, total, rel, result.Item.Reason, live, result.Err))
			continue
		}
		completed++
		pr.Status("OK", fmt.Sprintf("[%d/%d] %s (%s, %s)", done, total, rel, result.Item.Reason, live))
		// DownloadAll verified the content against the manifest hash before
		// moving it into place, so the result can be recorded as-is.
		if entry, ok := stateEntryFor(cfg.OutputDir, result.Item.Resource); ok {
			st.Resources[rel] = entry
		}
	}

	if err := state.Save(cfg.OutputDir, st); err != nil {
		return err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		remaining := total - completed - failed
		pr.Status("STOP", fmt.Sprintf("interrupted: %d completed, %d failed, %d not downloaded (%d in flight); progress saved", completed, failed, remaining, interrupted))
		return ctxErr
	}
	pr.Printf("Completed: %d, Failed: %d (hash mismatches: %d)\n", completed, failed, mismatched)
	if failed > 0 {
		return fmt.Errorf("%w: %d of %d downloads failed", errProblems, failed, total)
	}
	return nil
}

func runPlan(ctx context.Context, cfg config, pr *printer) error {
	dl := newDownloader(cfg)
	cfg, seeds, err := prepareSeeds(ctx, cfg, pr, true)
	if err != nil {
		return err
	}
	seeds, fetched, err := fetchSeeds(ctx, dl, cfg, seeds, false, pr)
	if err != nil {
		return err
	}

	resources, manifestsParsed, _ := loadResources(cfg, seeds, pr)
	plan, skipped, _, err := BuildDownloadPlan(ctx, cfg.OutputDir, resources, stateScope(cfg), cfg.Force, pr.Warnf)
	if err != nil {
		return err
	}

	printSummary("plan", model.Summary{
		SeedsFetched:     fetched,
		ManifestCount:    manifestsParsed,
		ResourceCount:    len(resources),
		PlannedDownloads: len(plan),
		SkippedResources: skipped,
	}, pr)
	printCategoryBreakdownResources(resources, "Resources by category", pr)
	printCategoryBreakdownPlan(plan, "Planned downloads by category", pr)
	printReasonBreakdown(plan, pr)

	for _, item := range sampleItems(plan, 25) {
		pr.Printf("DOWNLOAD %s (%s)\n", item.Resource.RelativePath, item.Reason)
	}
	if len(plan) > 25 {
		pr.Printf("... and %d more\n", len(plan)-25)
	}
	return nil
}

func runFetchManifests(ctx context.Context, cfg config, pr *printer) error {
	dl := newDownloader(cfg)
	cfg, seeds, err := prepareSeeds(ctx, cfg, pr, true)
	if err != nil {
		return err
	}
	_, fetched, err := fetchSeeds(ctx, dl, cfg, seeds, false, pr)
	if err != nil {
		return err
	}
	pr.Printf("Fetched %d manifest/template/core-metadata files into %s\n", fetched, cfg.OutputDir)
	return nil
}

func runVerify(ctx context.Context, cfg config, pr *printer) error {
	cfg, seeds, err := prepareSeeds(ctx, cfg, pr, false)
	if err != nil {
		return err
	}

	// Optional and language bootstrap seeds may legitimately not exist on
	// the server (sync skips them with a warning), so their absence is
	// reported but not counted as a problem.
	seedMissing := make([]string, 0)
	optionalMissing := make([]string, 0)
	for _, seed := range seeds {
		if !matchesCategory(seed.Category, cfg.Categories) || !discovery.MatchesLanguagePath(seed.RelativePath, cfg.Languages) {
			continue
		}
		localPath, err := safepath.Join(cfg.OutputDir, seed.RelativePath)
		if err != nil {
			pr.Warnf("skipping seed: %v", err)
			continue
		}
		if _, err := os.Stat(localPath); err != nil {
			if discovery.IsOptionalSeed(seed) || isLanguageBootstrapSeed(seed.RelativePath) {
				optionalMissing = append(optionalMissing, seed.RelativePath)
			} else {
				seedMissing = append(seedMissing, seed.RelativePath)
			}
		}
	}

	resources, manifestsParsed, manifestErrors := loadResources(cfg, seeds, pr)
	report, err := verifyResources(ctx, cfg.OutputDir, resources, pr.Warnf)
	if err != nil {
		return err
	}

	pr.Title("Verify Summary")
	pr.KV("Seed files checked", countSelectedSeeds(seeds, cfg.Categories, cfg.Languages))
	pr.KV("Filecollection manifests parsed", manifestsParsed)
	pr.KV("Unparsable manifests", manifestErrors)
	pr.KV("Manifest-derived resources checked", len(resources))
	pr.KV("Missing required seed files", len(seedMissing))
	pr.KV("Missing optional seed files", len(optionalMissing))
	pr.KV("Missing resources", len(report.missing))
	pr.KV("Hash mismatches", len(report.mismatch))
	pr.KV("Unreadable files", len(report.unreadable))

	printGroupedPaths(report.missing, "Top missing groups", pr)
	printGroupedPaths(report.mismatch, "Top hash mismatch groups", pr)
	printCategoryBreakdownResources(resources, "Resources by category", pr)
	printSample("Missing required seed files", seedMissing, 20, pr)
	printSample("Missing optional seed files (not an error)", optionalMissing, 20, pr)
	printSample("Missing resources", report.missing, 30, pr)
	printSample("Hash mismatches", report.mismatch, 30, pr)
	printSample("Unreadable files", report.unreadable, 30, pr)

	problems := len(seedMissing) + len(report.missing) + len(report.mismatch) + len(report.unreadable) + manifestErrors
	if problems > 0 {
		return fmt.Errorf("%w: verification found %d missing, mismatched, unreadable or unparsable files", errProblems, problems)
	}
	pr.Println("All checked files match their manifests.")
	return nil
}

// isLanguageBootstrapSeed reports whether rel is one of the two per-language
// bootstrap manifests added by discovery.AddLanguageBootstrapSeeds.
func isLanguageBootstrapSeed(rel string) bool {
	if strings.HasPrefix(rel, "spacemap/templates/language_") && strings.HasSuffix(rel, ".xml") && strings.Count(rel, "/") == 2 {
		return true
	}
	parts := strings.Split(rel, "/")
	return len(parts) == 4 && parts[0] == "do_img" && parts[1] != "global" && parts[2] == "xml" && parts[3] == "resource_localized.xml"
}

// fetchSeeds downloads the selected seeds concurrently. It returns the seeds
// minus the ones that were dropped (optional or language bootstrap seeds the
// server reports missing, and unsafe paths), and how many were fetched. A
// missing required seed (core files, spacemap/xml manifests) is fatal.
func fetchSeeds(ctx context.Context, dl *downloader.Client, cfg config, seeds []model.Seed, includeCoreBinary bool, pr *printer) ([]model.Seed, int, error) {
	bySeedPath := map[string]model.Seed{}
	items := make([]model.DownloadItem, 0, len(seeds))
	dropped := map[string]bool{}
	for _, seed := range discovery.FilterSeeds(seeds, cfg.Categories, includeCoreBinary) {
		if !includeCoreBinary && (strings.HasSuffix(seed.RelativePath, ".swf") || seed.RelativePath == "crossdomain.xml") {
			continue
		}
		if !discovery.MatchesLanguagePath(seed.RelativePath, cfg.Languages) {
			continue
		}
		if _, err := safepath.Join(cfg.OutputDir, seed.RelativePath); err != nil {
			pr.Warnf("skipping seed: %v", err)
			dropped[seed.RelativePath] = true
			continue
		}
		bySeedPath[seed.RelativePath] = seed
		items = append(items, model.DownloadItem{
			Resource: model.Resource{
				RelativePath: seed.RelativePath,
				URL:          manifest.JoinURL(cfg.BaseURL, seed.RelativePath, ""),
				Category:     seed.Category,
			},
			Reason: "seed",
		})
	}

	fetched := 0
	var fatal []error
	for result := range dl.DownloadAll(ctx, cfg.OutputDir, items, cfg.Concurrency) {
		rel := result.Item.Resource.RelativePath
		if result.Err == nil {
			fetched++
			pr.Status("FETCH", rel)
			continue
		}
		if errors.Is(result.Err, context.Canceled) {
			continue
		}
		seed := bySeedPath[rel]
		var statusErr *downloader.HTTPStatusError
		switch {
		case errors.As(result.Err, &statusErr) && statusErr.StatusCode == http.StatusNotFound && discovery.IsOptionalSeed(seed):
			pr.Status("SKIP", fmt.Sprintf("%s (optional seed missing on this site)", rel))
			dropped[rel] = true
		case errors.As(result.Err, &statusErr) && statusErr.StatusCode == http.StatusNotFound && isLanguageBootstrapSeed(rel):
			pr.Warnf("%s not found on the server; skipping this language file", rel)
			dropped[rel] = true
		case errors.Is(result.Err, safepath.ErrUnsafePath):
			pr.Warnf("skipping seed: %v", result.Err)
			dropped[rel] = true
		default:
			fatal = append(fatal, fmt.Errorf("fetch seed %s: %w", rel, result.Err))
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, fetched, err
	}
	if len(fatal) > 0 {
		sort.Slice(fatal, func(i, j int) bool { return fatal[i].Error() < fatal[j].Error() })
		if len(fatal) == 1 {
			return nil, fetched, fatal[0]
		}
		return nil, fetched, fmt.Errorf("%w (and %d more required seeds failed)", fatal[0], len(fatal)-1)
	}

	kept := make([]model.Seed, 0, len(seeds))
	for _, seed := range seeds {
		if !dropped[seed.RelativePath] {
			kept = append(kept, seed)
		}
	}
	return kept, fetched, nil
}

// loadResources parses the selected filecollection manifests and returns the
// deduplicated resources, the number of manifests parsed and the number of
// manifests that could not be parsed. Unparsable manifests and rejected
// entries are reported as warnings and skipped.
func loadResources(cfg config, seeds []model.Seed, pr *printer) ([]model.Resource, int, int) {
	resourceMap := map[string]model.Resource{}
	manifestsParsed, manifestErrors, skippedEntries := 0, 0, 0

	for _, seed := range seeds {
		if !matchesCategory(seed.Category, cfg.Categories) || !discovery.MatchesLanguagePath(seed.RelativePath, cfg.Languages) {
			continue
		}
		localPath, err := safepath.Join(cfg.OutputDir, seed.RelativePath)
		if err != nil {
			pr.Warnf("skipping manifest: %v", err)
			continue
		}
		result, ok, err := manifest.Load(localPath, seed.RelativePath, cfg.BaseURL, seed.Category)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			manifestErrors++
			pr.Warnf("skipping manifest %s: %v", seed.RelativePath, err)
			continue
		}
		if !ok {
			continue
		}
		manifestsParsed++
		if len(result.Skipped) > 0 {
			skippedEntries += len(result.Skipped)
			first := result.Skipped[0]
			pr.Warnf("%s: skipped %d invalid or unsafe entries (e.g. %q: %s)", seed.RelativePath, len(result.Skipped), first.Path, first.Reason)
		}
		for _, resource := range result.Resources {
			if !matchesCategory(resource.Category, cfg.Categories) || !discovery.MatchesLanguagePath(resource.RelativePath, cfg.Languages) {
				continue
			}
			// Windows and macOS filesystems are case-insensitive, so paths
			// differing only in case refer to the same file.
			key := strings.ToLower(resource.RelativePath)
			if existing, exists := resourceMap[key]; exists {
				if existing.Hash == "" && resource.Hash != "" {
					resourceMap[key] = resource
				}
				continue
			}
			resourceMap[key] = resource
		}
	}
	if skippedEntries > 0 {
		pr.Warnf("%d manifest entries were skipped in total", skippedEntries)
	}

	resources := make([]model.Resource, 0, len(resourceMap))
	for _, resource := range resourceMap {
		resources = append(resources, resource)
	}
	sort.Slice(resources, func(i, j int) bool {
		return resources[i].RelativePath < resources[j].RelativePath
	})
	return resources, manifestsParsed, manifestErrors
}

// stateEntryFor describes the local file of resource for the state file.
func stateEntryFor(outputDir string, resource model.Resource) (state.StateEntry, bool) {
	localPath, err := safepath.Join(outputDir, resource.RelativePath)
	if err != nil {
		return state.StateEntry{}, false
	}
	info, err := os.Stat(localPath)
	if err != nil {
		return state.StateEntry{}, false
	}
	return state.StateEntry{
		Hash:     resource.Hash,
		URL:      resource.URL,
		Category: resource.Category,
		Size:     info.Size(),
		ModTime:  info.ModTime().UnixNano(),
	}, true
}

// stateScope returns the predicate BuildDownloadPlan uses to decide which
// previous state entries the current --category/--languages selection covers.
// An entry is in scope when its path matches the selected languages and its
// recorded category is selected. Entries written by older versions have no
// category; they are only in scope when every category is selected, so a
// filtered sync never drops an entry it cannot attribute to the filter.
func stateScope(cfg config) func(rel string, entry state.StateEntry) bool {
	allCategories := len(cfg.Categories) == 0 || cfg.Categories["all"]
	return func(rel string, entry state.StateEntry) bool {
		if !discovery.MatchesLanguagePath(rel, cfg.Languages) {
			return false
		}
		if entry.Category == "" {
			return allCategories
		}
		return matchesCategory(entry.Category, cfg.Categories)
	}
}

// BuildDownloadPlan decides which resources need downloading. A file whose
// size and modification time still match its state entry, and whose manifest
// hash is unchanged, is trusted without re-hashing; otherwise it is hashed.
// With force every resource is planned.
//
// The returned state starts as a copy of the previous state. Entries for
// up-to-date resources are refreshed; entries for planned resources are left
// as they were until the download succeeds. A previous entry whose path is no
// longer among resources is dropped only when inScope reports that the
// current selection covers it (nil inScope covers everything), so a filtered
// sync keeps the entries of resources outside its filter. Unsafe paths and
// unreadable state are reported through warn.
func BuildDownloadPlan(ctx context.Context, outputDir string, resources []model.Resource, inScope func(rel string, entry state.StateEntry) bool, force bool, warn func(format string, args ...any)) ([]model.DownloadItem, int, *state.State, error) {
	previous, err := state.Load(outputDir)
	if err != nil {
		if !errors.Is(err, state.ErrCorrupt) {
			return nil, 0, nil, err
		}
		warn("%v; starting with an empty state", err)
	}

	current := make(map[string]bool, len(resources))
	for _, resource := range resources {
		current[resource.RelativePath] = true
	}
	st := &state.State{Resources: make(map[string]state.StateEntry, len(previous.Resources)+len(resources))}
	for rel, entry := range previous.Resources {
		if !current[rel] && (inScope == nil || inScope(rel, entry)) {
			continue
		}
		st.Resources[rel] = entry
	}

	items := make([]model.DownloadItem, 0)
	skipped := 0
	for _, resource := range resources {
		if err := ctx.Err(); err != nil {
			return nil, 0, nil, err
		}
		localPath, err := safepath.Join(outputDir, resource.RelativePath)
		if err != nil {
			warn("skipping resource: %v", err)
			continue
		}

		prev, hasPrev := previous.Resources[resource.RelativePath]
		info, statErr := os.Stat(localPath)
		reason := ""
		switch {
		case force:
			reason = "forced"
		case statErr != nil:
			if errors.Is(statErr, fs.ErrNotExist) {
				reason = "missing"
			} else {
				reason = "unreadable"
			}
		case !info.Mode().IsRegular():
			reason = "unreadable"
		case resource.Hash != "":
			unchanged := hasPrev && strings.EqualFold(prev.Hash, resource.Hash) &&
				prev.Size == info.Size() && prev.ModTime == info.ModTime().UnixNano()
			if unchanged {
				break
			}
			match, err := integrity.FileMatchesManifestHash(localPath, resource.Hash)
			switch {
			case err != nil:
				reason = "unreadable"
			case !match:
				reason = "hash-mismatch"
			}
		case hasPrev && prev.URL != "" && prev.URL != resource.URL:
			reason = "url-changed"
		}

		if reason != "" {
			items = append(items, model.DownloadItem{Resource: resource, Reason: reason})
			continue
		}
		skipped++
		st.Resources[resource.RelativePath] = state.StateEntry{
			Hash:     resource.Hash,
			URL:      resource.URL,
			Category: resource.Category,
			Size:     info.Size(),
			ModTime:  info.ModTime().UnixNano(),
		}
	}
	return items, skipped, st, nil
}

type verifyReport struct {
	missing    []string
	mismatch   []string
	unreadable []string
}

// verifyResources fully hashes every local resource that has a manifest hash.
func verifyResources(ctx context.Context, outputDir string, resources []model.Resource, warn func(format string, args ...any)) (verifyReport, error) {
	var report verifyReport
	for _, resource := range resources {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		localPath, err := safepath.Join(outputDir, resource.RelativePath)
		if err != nil {
			warn("skipping resource: %v", err)
			continue
		}
		if _, err := os.Stat(localPath); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				report.missing = append(report.missing, resource.RelativePath)
			} else {
				report.unreadable = append(report.unreadable, resource.RelativePath)
			}
			continue
		}
		if resource.Hash == "" {
			continue
		}
		match, err := integrity.FileMatchesManifestHash(localPath, resource.Hash)
		switch {
		case err != nil:
			report.unreadable = append(report.unreadable, resource.RelativePath)
		case !match:
			report.mismatch = append(report.mismatch, resource.RelativePath)
		}
	}
	return report, nil
}

func resolveLanguages(ctx context.Context, cfg config, seeds []model.Seed, pr *printer) (map[string]bool, error) {
	if len(cfg.Languages) == 0 {
		return map[string]bool{"en": true}, nil
	}
	if !cfg.Languages["all"] {
		return cfg.Languages, nil
	}

	live, failures, err := discovery.DiscoverLiveLanguageReport(ctx, cfg.BaseURL)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	for _, failure := range failures {
		pr.Warnf("could not probe language %s: %v", failure.Language, failure.Err)
	}
	if err == nil && len(live) > 0 {
		resolved := map[string]bool{}
		for _, lang := range live {
			resolved[lang] = true
		}
		pr.Status("DISCOVER", fmt.Sprintf("live languages: %s", strings.Join(live, ", ")))
		return resolved, nil
	}
	if err != nil {
		pr.Warnf("live language discovery failed: %v", err)
	}

	fallback := discovery.ResolveBootstrapLanguages(cfg.OutputDir, seeds, cfg.Languages)
	resolved := map[string]bool{}
	for _, lang := range fallback {
		resolved[lang] = true
	}
	if len(fallback) > 0 {
		pr.Status("DISCOVER", fmt.Sprintf("fallback languages: %s", strings.Join(fallback, ", ")))
		return resolved, nil
	}
	return resolved, err
}

func prepareSeeds(ctx context.Context, cfg config, pr *printer, probeOptional bool) (config, []model.Seed, error) {
	seeds, err := discovery.DiscoverSeeds(cfg.OutputDir)
	if err != nil {
		return cfg, nil, err
	}

	resolvedLanguages, err := resolveLanguages(ctx, cfg, seeds, pr)
	if err != nil {
		return cfg, nil, err
	}
	cfg.Languages = resolvedLanguages
	seeds = discovery.AddLanguageBootstrapSeeds(cfg.OutputDir, seeds, cfg.Languages)

	if !probeOptional {
		return cfg, seeds, nil
	}

	probedSeeds, skipped, err := discovery.DiscoverAvailableSeeds(ctx, cfg.BaseURL, seeds)
	if err != nil {
		return cfg, nil, err
	}
	if len(skipped) > 0 {
		pr.Status("DISCOVER", fmt.Sprintf("skipping %d optional seeds that are missing on this site", len(skipped)))
	}
	return cfg, probedSeeds, nil
}

func countSelectedSeeds(seeds []model.Seed, allowed, languages map[string]bool) int {
	count := 0
	for _, seed := range seeds {
		if matchesCategory(seed.Category, allowed) && discovery.MatchesLanguagePath(seed.RelativePath, languages) {
			count++
		}
	}
	return count
}

func newDownloader(cfg config) *downloader.Client {
	return downloader.NewWithConfig(downloader.Config{
		StallTimeout:        30 * time.Second,
		RequestInterval:     cfg.RequestInterval,
		MaxCooldown:         20 * time.Second,
		MaxConcurrency:      cfg.Concurrency,
		MinConcurrency:      cfg.MinConcurrency,
		AutoTuneConcurrency: cfg.AutoTuneConcurrency,
	})
}
