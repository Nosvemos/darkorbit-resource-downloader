package model

type Seed struct {
	RelativePath string
	Category     string
	// Required seeds abort a sync when they cannot be fetched; the others
	// are skipped when the server reports them missing.
	Required bool
}

type Resource struct {
	RelativePath string
	URL          string
	Category     string
	ManifestPath string
	Hash         string
	Type         string
}

type DownloadItem struct {
	Resource Resource
	Reason   string
}

type Summary struct {
	SeedsFetched     int
	ManifestCount    int
	ResourceCount    int
	PlannedDownloads int
	SkippedResources int
}
