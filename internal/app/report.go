package app

import (
	"sort"
	"strings"

	"github.com/Nosvemos/Darkorbit-Resource-Downloader/internal/model"
)

func sampleItems(items []model.DownloadItem, limit int) []model.DownloadItem {
	if len(items) <= limit {
		return items
	}
	return items[:limit]
}

func printSummary(command string, summary model.Summary, pr *printer) {
	pr.Title(titleCase(command) + " Summary")
	pr.KV("Seed files fetched", summary.SeedsFetched)
	pr.KV("Filecollection manifests parsed", summary.ManifestCount)
	pr.KV("Manifest-derived resources", summary.ResourceCount)
	pr.KV("Planned downloads", summary.PlannedDownloads)
	pr.KV("Skipped existing", summary.SkippedResources)
}

type countPair struct {
	Key   string
	Count int
}

// sortedCounts orders counts by descending count, then key.
func sortedCounts(counts map[string]int) []countPair {
	pairs := make([]countPair, 0, len(counts))
	for key, count := range counts {
		pairs = append(pairs, countPair{Key: key, Count: count})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].Count == pairs[j].Count {
			return pairs[i].Key < pairs[j].Key
		}
		return pairs[i].Count > pairs[j].Count
	})
	return pairs
}

func printGroupedPaths(paths []string, title string, pr *printer) {
	grouped := map[string]int{}
	for _, rel := range paths {
		grouped[groupKey(rel)]++
	}
	pairs := sortedCounts(grouped)
	if len(pairs) == 0 {
		return
	}
	pairs = pairs[:min(15, len(pairs))]
	pr.Section(title)
	for _, item := range pairs {
		pr.Printf("%4d  %s\n", item.Count, item.Key)
	}
}

func printCategoryBreakdownResources(resources []model.Resource, title string, pr *printer) {
	counts := map[string]int{}
	for _, resource := range resources {
		counts[resource.Category]++
	}
	printCounts(title, counts, pr)
}

func printCategoryBreakdownPlan(plan []model.DownloadItem, title string, pr *printer) {
	counts := map[string]int{}
	for _, item := range plan {
		counts[item.Resource.Category]++
	}
	printCounts(title, counts, pr)
}

func printReasonBreakdown(plan []model.DownloadItem, pr *printer) {
	counts := map[string]int{}
	for _, item := range plan {
		counts[item.Reason]++
	}
	printCounts("Planned downloads by reason", counts, pr)
}

func printCounts(title string, counts map[string]int, pr *printer) {
	if len(counts) == 0 {
		return
	}
	pr.Section(title)
	for _, item := range sortedCounts(counts) {
		pr.Printf("%6d  %s\n", item.Count, item.Key)
	}
}

func groupKey(rel string) string {
	parts := strings.Split(rel, "/")
	switch {
	case len(parts) >= 3 && parts[0] == "spacemap" && (parts[1] == "3d" || parts[1] == "graphics"):
		return strings.Join(parts[:3], "/")
	case len(parts) >= 2:
		return strings.Join(parts[:2], "/")
	}
	return rel
}

func printSample(title string, items []string, limit int, pr *printer) {
	if len(items) == 0 {
		return
	}
	pr.Section(title)
	limit = min(limit, len(items))
	for _, item := range items[:limit] {
		pr.Printf("  %s\n", item)
	}
	if len(items) > limit {
		pr.Printf("  ... and %d more\n", len(items)-limit)
	}
}
