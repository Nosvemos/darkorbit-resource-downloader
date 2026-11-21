package downloader_test

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Nosvemos/Darkorbit-Resource-Downloader/internal/downloader"
	"github.com/Nosvemos/Darkorbit-Resource-Downloader/internal/model"
	"github.com/Nosvemos/Darkorbit-Resource-Downloader/internal/safepath"
)

// fastConfig keeps retries and cooldowns short so tests do not sleep for
// seconds.
func fastConfig() downloader.Config {
	return downloader.Config{
		StallTimeout:   5 * time.Second,
		MaxCooldown:    50 * time.Millisecond,
		MaxConcurrency: 1,
		RetryBaseDelay: 5 * time.Millisecond,
		RetryMaxDelay:  20 * time.Millisecond,
	}
}

func item(server *httptest.Server, rel, hash string) model.DownloadItem {
	return model.DownloadItem{Resource: model.Resource{RelativePath: rel, URL: server.URL + "/" + rel, Hash: hash}}
}

// listFiles returns every regular file under root, relative and slash-separated.
func listFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			rel, _ := filepath.Rel(root, path)
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func collect(results <-chan downloader.Result) map[string]error {
	errs := map[string]error{}
	for r := range results {
		errs[r.Item.Resource.RelativePath] = r.Err
	}
	return errs
}

func TestFetchToFilePersistsXMLAndTextResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifest.xml":
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte("<filecollection></filecollection>"))
		case "/endpoint.txt":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("plain text content"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := downloader.New()
	dir := t.TempDir()

	xmlDest := filepath.Join(dir, "spacemap", "xml", "resources.xml")
	if err := client.FetchToFile(context.Background(), server.URL+"/manifest.xml", xmlDest); err != nil {
		t.Fatal(err)
	}
	xmlData, err := os.ReadFile(xmlDest)
	if err != nil {
		t.Fatal(err)
	}
	if string(xmlData) != "<filecollection></filecollection>" {
		t.Fatalf("unexpected xml content: %q", string(xmlData))
	}

	txtDest := filepath.Join(dir, "unityApi", "events", "spaceSearch.txt")
	if err := client.FetchToFile(context.Background(), server.URL+"/endpoint.txt", txtDest); err != nil {
		t.Fatal(err)
	}
	txtData, err := os.ReadFile(txtDest)
	if err != nil {
		t.Fatal(err)
	}
	if string(txtData) != "plain text content" {
		t.Fatalf("unexpected text content: %q", string(txtData))
	}

	if files := listFiles(t, dir); len(files) != 2 {
		t.Fatalf("expected exactly the two downloaded files, got %v", files)
	}
}

func TestFetchToFileRetries503AndSucceeds(t *testing.T) {
	var attempts atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) < 3 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	client := downloader.NewWithConfig(fastConfig())
	dest := filepath.Join(t.TempDir(), "retry.txt")

	if err := client.FetchToFile(context.Background(), server.URL+"/retry", dest); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts.Load())
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "ok" {
		t.Fatalf("unexpected downloaded content: %q", string(data))
	}
}

func TestFetchToFileDoesNotRetry404(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		http.NotFound(w, r)
	}))
	defer server.Close()

	dir := t.TempDir()
	err := downloader.NewWithConfig(fastConfig()).FetchToFile(context.Background(), server.URL+"/missing", filepath.Join(dir, "missing.bin"))

	var statusErr *downloader.HTTPStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 HTTPStatusError, got %v", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("expected exactly 1 attempt for 404, got %d", got)
	}
	if files := listFiles(t, dir); len(files) != 0 {
		t.Fatalf("expected no files, got %v", files)
	}
}

func TestFetchToFileHonours429RetryAfter(t *testing.T) {
	tests := []struct {
		name   string
		header func() string
	}{
		{"seconds", func() string { return "1" }},
		{"http-date", func() string { return time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var starts []time.Time
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				starts = append(starts, time.Now())
				n := len(starts)
				mu.Unlock()
				if n == 1 {
					w.Header().Set("Retry-After", tt.header())
					http.Error(w, "slow down", http.StatusTooManyRequests)
					return
				}
				_, _ = w.Write([]byte("ok"))
			}))
			defer server.Close()

			dest := filepath.Join(t.TempDir(), "file.bin")
			if err := downloader.NewWithConfig(fastConfig()).FetchToFile(context.Background(), server.URL+"/file", dest); err != nil {
				t.Fatal(err)
			}

			mu.Lock()
			defer mu.Unlock()
			if len(starts) != 2 {
				t.Fatalf("expected 2 attempts, got %d", len(starts))
			}
			// Without Retry-After the configured backoff would be ~5ms.
			// HTTP-date has second granularity, so allow for truncation.
			if gap := starts[1].Sub(starts[0]); gap < 900*time.Millisecond {
				t.Fatalf("expected retry to wait for Retry-After, waited only %s", gap)
			}
		})
	}
}

func TestFetchToFileContextCancelledMidDownload(t *testing.T) {
	var attempts atomic.Int32
	firstBytesSent := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) > 1 {
			http.Error(w, "unexpected retry", http.StatusTeapot)
			return
		}
		w.Header().Set("Content-Length", "1000000")
		_, _ = w.Write([]byte(strings.Repeat("x", 1024)))
		w.(http.Flusher).Flush()
		close(firstBytesSent)
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-firstBytesSent
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	dir := t.TempDir()
	err := downloader.NewWithConfig(fastConfig()).FetchToFile(ctx, server.URL+"/big", filepath.Join(dir, "big.bin"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("expected no retry after cancellation, got %d attempts", got)
	}
	if files := listFiles(t, dir); len(files) != 0 {
		t.Fatalf("expected temp file to be removed, found %v", files)
	}
}

func TestFetchToFileRemovesTempFileOnTruncatedBody(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		// Promise more bytes than are sent so the client sees an unexpected EOF.
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("short"))
	}))
	defer server.Close()

	dir := t.TempDir()
	err := downloader.NewWithConfig(fastConfig()).FetchToFile(context.Background(), server.URL+"/short", filepath.Join(dir, "short.bin"))
	if err == nil {
		t.Fatal("expected error for truncated body")
	}
	if got := attempts.Load(); got != 6 {
		t.Fatalf("expected truncated bodies to be retried 6 times, got %d", got)
	}
	if files := listFiles(t, dir); len(files) != 0 {
		t.Fatalf("expected no files after failure, found %v", files)
	}
}

func TestFetchToFileStallTimeout(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()

	cfg := fastConfig()
	cfg.StallTimeout = 100 * time.Millisecond
	dir := t.TempDir()
	err := downloader.NewWithConfig(cfg).FetchToFile(context.Background(), server.URL+"/stall", filepath.Join(dir, "stall.bin"))
	if !errors.Is(err, downloader.ErrStalled) {
		t.Fatalf("expected ErrStalled, got %v", err)
	}
	if got := attempts.Load(); got != 6 {
		t.Fatalf("expected stalls to be retried 6 times, got %d", got)
	}
	if files := listFiles(t, dir); len(files) != 0 {
		t.Fatalf("expected no files after failure, found %v", files)
	}
}

func TestFetchToFileDoesNotRetryLocalFilesystemErrors(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	dir := t.TempDir()
	// A regular file where a parent directory is needed makes MkdirAll fail.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	err := downloader.NewWithConfig(fastConfig()).FetchToFile(context.Background(), server.URL+"/x", filepath.Join(blocker, "sub", "x.bin"))
	if err == nil {
		t.Fatal("expected local filesystem error")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("expected local filesystem error not to be retried, got %d attempts", got)
	}
}

func TestDownloadAllVerifiesHash(t *testing.T) {
	var badAttempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad.bin" {
			badAttempts.Add(1)
		}
		_, _ = w.Write([]byte("hello"))
	}))
	defer server.Close()

	dir := t.TempDir()
	items := []model.DownloadItem{
		item(server, "normalized.bin", "5d41402abc4b2a76b9719d911017c500"),
		item(server, "full-upper.bin", "5D41402ABC4B2A76B9719D911017C592"),
		item(server, "nohash.bin", ""),
		item(server, "bad.bin", "00000000000000000000000000000000"),
	}
	errs := collect(downloader.NewWithConfig(fastConfig()).DownloadAll(context.Background(), dir, items, 2))

	for _, rel := range []string{"normalized.bin", "full-upper.bin", "nohash.bin"} {
		if errs[rel] != nil {
			t.Fatalf("unexpected error for %s: %v", rel, errs[rel])
		}
	}

	var mismatch *downloader.HashMismatchError
	if !errors.Is(errs["bad.bin"], downloader.ErrHashMismatch) || !errors.As(errs["bad.bin"], &mismatch) {
		t.Fatalf("expected hash mismatch for bad.bin, got %v", errs["bad.bin"])
	}
	if mismatch.Actual != "5d41402abc4b2a76b9719d911017c592" {
		t.Fatalf("unexpected actual hash %q", mismatch.Actual)
	}
	if got := badAttempts.Load(); got != 2 {
		t.Fatalf("expected one retry on hash mismatch (2 attempts), got %d", got)
	}

	files := listFiles(t, dir)
	slices.Sort(files)
	if want := []string{"full-upper.bin", "nohash.bin", "normalized.bin"}; !slices.Equal(files, want) {
		t.Fatalf("unexpected files %v, want %v", files, want)
	}
}

func TestDownloadAllRejectsPathEscape(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("payload"))
	}))
	defer server.Close()

	parent := t.TempDir()
	outputDir := filepath.Join(parent, "out")
	items := []model.DownloadItem{
		{Resource: model.Resource{RelativePath: "../evil.bin", URL: server.URL + "/evil"}},
		{Resource: model.Resource{RelativePath: `..\evil2.bin`, URL: server.URL + "/evil2"}},
		{Resource: model.Resource{RelativePath: "/abs.bin", URL: server.URL + "/abs"}},
		{Resource: model.Resource{RelativePath: "good.bin", URL: server.URL + "/good"}},
	}
	errs := collect(downloader.NewWithConfig(fastConfig()).DownloadAll(context.Background(), outputDir, items, 2))

	for _, rel := range []string{"../evil.bin", `..\evil2.bin`, "/abs.bin"} {
		if !errors.Is(errs[rel], safepath.ErrUnsafePath) {
			t.Fatalf("expected ErrUnsafePath for %q, got %v", rel, errs[rel])
		}
	}
	if errs["good.bin"] != nil {
		t.Fatalf("unexpected error for good.bin: %v", errs["good.bin"])
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("expected only the safe item to be requested, got %d requests", got)
	}
	if files := listFiles(t, parent); !slices.Equal(files, []string{"out/good.bin"}) {
		t.Fatalf("unexpected files under parent: %v", files)
	}
}

func TestDownloadAllPacesConcurrentRequests(t *testing.T) {
	const (
		interval = 80 * time.Millisecond
		minGap   = 50 * time.Millisecond
	)
	var tooFast atomic.Int32
	var mu sync.Mutex
	var starts []time.Time

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		mu.Lock()
		if n := len(starts); n > 0 && now.Sub(starts[n-1]) < minGap {
			tooFast.Add(1)
		}
		starts = append(starts, now)
		mu.Unlock()
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	cfg := fastConfig()
	cfg.RequestInterval = interval
	cfg.MaxConcurrency = 3
	client := downloader.NewWithConfig(cfg)

	dir := t.TempDir()
	items := []model.DownloadItem{
		item(server, "one.bin", ""),
		item(server, "two.bin", ""),
		item(server, "three.bin", ""),
		item(server, "four.bin", ""),
	}
	for rel, err := range collect(client.DownloadAll(context.Background(), dir, items, 3)) {
		if err != nil {
			t.Fatalf("unexpected download error for %s: %v", rel, err)
		}
	}

	if got := tooFast.Load(); got != 0 {
		t.Fatalf("expected no requests closer than %s, got %d", minGap, got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(starts) != len(items) {
		t.Fatalf("expected %d requests, got %d", len(items), len(starts))
	}
	slices.SortFunc(starts, func(a, b time.Time) int { return a.Compare(b) })
	smallest := time.Duration(1<<63 - 1)
	for i := 1; i < len(starts); i++ {
		smallest = min(smallest, starts[i].Sub(starts[i-1]))
	}
	if smallest < minGap {
		t.Fatalf("minimum gap between request starts was %s, want >= %s", smallest, minGap)
	}
}

// throttlingServer returns 503 for the first two attempts of any path that
// arrives while more than two requests are in flight.
func throttlingServer(throttles *atomic.Int32, maxObserved *atomic.Int32) *httptest.Server {
	var inFlight atomic.Int32
	var mu sync.Mutex
	attemptsByPath := map[string]int{}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			previous := maxObserved.Load()
			if current <= previous || maxObserved.CompareAndSwap(previous, current) {
				break
			}
		}

		mu.Lock()
		attemptsByPath[r.URL.Path]++
		attempt := attemptsByPath[r.URL.Path]
		mu.Unlock()

		time.Sleep(40 * time.Millisecond)
		if current > 2 && attempt <= 2 {
			throttles.Add(1)
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
}

func autoTuneItems(server *httptest.Server) []model.DownloadItem {
	return []model.DownloadItem{
		item(server, "one.bin", ""),
		item(server, "two.bin", ""),
		item(server, "three.bin", ""),
		item(server, "four.bin", ""),
		item(server, "five.bin", ""),
		item(server, "six.bin", ""),
	}
}

func TestDownloadAllAutoTunesConcurrencyAfter503(t *testing.T) {
	var throttles, maxObserved atomic.Int32
	server := throttlingServer(&throttles, &maxObserved)
	defer server.Close()

	cfg := fastConfig()
	cfg.MaxConcurrency = 4
	cfg.MinConcurrency = 1
	cfg.AutoTuneConcurrency = true
	client := downloader.NewWithConfig(cfg)

	lowest := client.Stats().EffectiveConcurrency
	if lowest != 4 {
		t.Fatalf("expected initial effective concurrency 4, got %d", lowest)
	}

	dir := t.TempDir()
	for r := range client.DownloadAll(context.Background(), dir, autoTuneItems(server), 4) {
		if r.Err != nil {
			t.Fatalf("unexpected download error for %s: %v", r.Item.Resource.RelativePath, r.Err)
		}
		lowest = min(lowest, client.Stats().EffectiveConcurrency)
	}
	final := client.Stats()
	lowest = min(lowest, final.EffectiveConcurrency)

	if throttles.Load() == 0 {
		t.Fatal("expected the test server to issue at least one throttle response")
	}
	if maxObserved.Load() < 3 {
		t.Fatalf("expected initial concurrency pressure above 2, got %d", maxObserved.Load())
	}
	// Each 503 lowers the limit by one and raising it again needs more
	// consecutive successes than there are items, so the drop must persist.
	if lowest >= 4 || final.EffectiveConcurrency >= final.MaxConcurrency {
		t.Fatalf("expected effective concurrency to drop below 4, lowest %d, final %d", lowest, final.EffectiveConcurrency)
	}
	if final.EffectiveConcurrency < final.MinConcurrency {
		t.Fatalf("effective concurrency %d fell below minimum %d", final.EffectiveConcurrency, final.MinConcurrency)
	}
	if final.InFlight != 0 {
		t.Fatalf("expected no in-flight requests after completion, got %d", final.InFlight)
	}
	if files := listFiles(t, dir); len(files) != 6 {
		t.Fatalf("expected 6 files, got %v", files)
	}
}

func TestDownloadAllWithoutAutoTuneKeepsConcurrency(t *testing.T) {
	var throttles, maxObserved atomic.Int32
	server := throttlingServer(&throttles, &maxObserved)
	defer server.Close()

	cfg := fastConfig()
	cfg.MaxConcurrency = 4
	cfg.MinConcurrency = 1
	cfg.AutoTuneConcurrency = false
	client := downloader.NewWithConfig(cfg)

	dir := t.TempDir()
	for r := range client.DownloadAll(context.Background(), dir, autoTuneItems(server), 4) {
		if r.Err != nil {
			t.Fatalf("unexpected download error for %s: %v", r.Item.Resource.RelativePath, r.Err)
		}
		if got := client.Stats().EffectiveConcurrency; got != 4 {
			t.Fatalf("effective concurrency changed to %d with auto-tune off", got)
		}
	}
	if throttles.Load() == 0 {
		t.Fatal("expected the test server to issue at least one throttle response")
	}
	final := client.Stats()
	if final.EffectiveConcurrency != 4 || final.AutoTuneEnabled {
		t.Fatalf("unexpected final stats with auto-tune off: %+v", final)
	}
}

func TestRemoveStaleParts(t *testing.T) {
	dir := t.TempDir()
	write := func(rel string) {
		t.Helper()
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("keep.bin")
	write("a.bin.123456.part")
	write("sub/deep/foo.png.123456.part")
	write("sub/keep.partial")
	// Files that merely end in ".part" are not this tool's temp files.
	write("notes.part")
	write("sub/video.mp4.part")
	write("sub/.123.part")
	// A name produced by the same os.CreateTemp pattern the downloader uses.
	tmp, err := os.CreateTemp(filepath.Join(dir, "sub"), "c.swf.*.part")
	if err != nil {
		t.Fatal(err)
	}
	_ = tmp.Close()

	removed, err := downloader.RemoveStaleParts(dir)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 3 {
		t.Fatalf("expected 3 removed, got %d", removed)
	}
	files := listFiles(t, dir)
	slices.Sort(files)
	if want := []string{"keep.bin", "notes.part", "sub/.123.part", "sub/keep.partial", "sub/video.mp4.part"}; !slices.Equal(files, want) {
		t.Fatalf("unexpected remaining files %v, want %v", files, want)
	}

	if n, err := downloader.RemoveStaleParts(filepath.Join(dir, "does-not-exist")); err != nil || n != 0 {
		t.Fatalf("expected missing root to be a no-op, got %d, %v", n, err)
	}
}
