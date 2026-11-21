package app

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Nosvemos/Darkorbit-Resource-Downloader/internal/integrity"
	"github.com/Nosvemos/Darkorbit-Resource-Downloader/internal/model"
	"github.com/Nosvemos/Darkorbit-Resource-Downloader/internal/state"
)

func manifestHash(content []byte) string {
	sum := md5.Sum(content)
	return integrity.Normalize(hex.EncodeToString(sum[:]))
}

// fakeSite serves the hard-coded required seeds, a small resources.xml
// filecollection and its assets. Everything else is a 404.
type fakeSite struct {
	*httptest.Server
	mu    sync.Mutex
	files map[string][]byte
	gets  map[string]int
}

var (
	assetA = []byte("asset A content")
	assetB = []byte("asset B content, a little longer")
)

func newFakeSite(t *testing.T) *fakeSite {
	t.Helper()
	emptyCollection := []byte(`<?xml version="1.0" encoding="utf-8"?><filecollection></filecollection>`)
	resourcesXML := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<filecollection>
  <location id="gfx" path="graphics/"/>
  <file id="a" location="gfx" name="a" type="png" hash="%s"/>
  <file id="b" location="gfx" name="b" type="png" hash="%s"/>
  <file id="evil" location="gfx" name="../../../evil" type="txt" hash="%s"/>
</filecollection>`, manifestHash(assetA), manifestHash(assetB), manifestHash(assetA))

	site := &fakeSite{
		files: map[string][]byte{
			"/crossdomain.xml":                                []byte(`<?xml version="1.0"?><cross-domain-policy><allow-access-from domain="*"/></cross-domain-policy>`),
			"/spacemap/preloader.swf":                         []byte("FWS\x0apreloader"),
			"/spacemap/loadingscreen.swf":                     []byte("FWS\x0aloadingscreen"),
			"/spacemap/main.swf":                              []byte("FWS\x0amain"),
			"/spacemap/xml/profile.xml":                       []byte(`<?xml version="1.0"?><profile/>`),
			"/spacemap/xml/resources.xml":                     []byte(resourcesXML),
			"/spacemap/xml/resources_3d.xml":                  emptyCollection,
			"/spacemap/xml/resources_3d_particles.xml":        emptyCollection,
			"/spacemap/xml/assets_loadingScreen.xml":          emptyCollection,
			"/spacemap/graphics/a.png":                        assetA,
			"/spacemap/graphics/b.png":                        assetB,
			"/evil.txt":                                       assetA,
			"/spacemap/templates/language_en.xml":             []byte(`<?xml version="1.0"?><language/>`),
			"/do_img/global/xml/resource_items.xml":           emptyCollection,
			"/resources/command-center/positions.xml":         []byte(`<?xml version="1.0"?><positions/>`),
			"/do_img/global/xml/resource_achievements.xml":    emptyCollection,
			"/do_img/global/xml/resource_jumpgate.xml":        emptyCollection,
			"/do_img/global/xml/resource_events.xml":          emptyCollection,
			"/resources/resource_command_center.xml":          emptyCollection,
			"/spacemap/templates/unused/never_requested.html": []byte("x"),
		},
		gets: map[string]int{},
	}
	// do_img/en/xml/resource_localized.xml is deliberately missing: a language
	// bootstrap seed that 404s must only produce a warning.
	site.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		site.mu.Lock()
		body, ok := site.files[r.URL.Path]
		if r.Method == http.MethodGet {
			site.gets[r.URL.Path]++
		}
		site.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(site.Close)
	return site
}

func (s *fakeSite) getCount(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets[path]
}

func (s *fakeSite) remove(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.files, path)
}

func (s *fakeSite) set(path string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[path] = body
}

func loadState(t *testing.T, out string) *state.State {
	t.Helper()
	st, err := state.Load(out)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestFilteredSyncKeepsStateOutsideFilter(t *testing.T) {
	site := newFakeSite(t)
	out := filepath.Join(t.TempDir(), "mirror")

	res := runCLI(t, siteArgs("sync", out, site.URL)...)
	if res.code != ExitOK {
		t.Fatalf("full sync exit %d\n%s\n%s", res.code, res.stdout, res.stderr)
	}
	for _, rel := range []string{"spacemap/graphics/a.png", "spacemap/graphics/b.png"} {
		entry, ok := loadState(t, out).Resources[rel]
		if !ok || entry.Category != "spacemap" {
			t.Fatalf("after full sync, state entry %s = %+v (present %v)", rel, entry, ok)
		}
	}

	// A sync limited to another category must not wipe the spacemap entries.
	res = runCLI(t, siteArgs("sync", out, site.URL, "--category", "do_img")...)
	if res.code != ExitOK {
		t.Fatalf("filtered sync exit %d\n%s\n%s", res.code, res.stdout, res.stderr)
	}
	st := loadState(t, out)
	for _, rel := range []string{"spacemap/graphics/a.png", "spacemap/graphics/b.png"} {
		if _, ok := st.Resources[rel]; !ok {
			t.Errorf("filtered sync dropped the state entry for %s: %v", rel, st.Resources)
		}
	}
}

func TestSyncPrunesStateForRemovedResources(t *testing.T) {
	site := newFakeSite(t)
	out := filepath.Join(t.TempDir(), "mirror")

	res := runCLI(t, siteArgs("sync", out, site.URL)...)
	if res.code != ExitOK {
		t.Fatalf("first sync exit %d\n%s\n%s", res.code, res.stdout, res.stderr)
	}
	if _, ok := loadState(t, out).Resources["spacemap/graphics/b.png"]; !ok {
		t.Fatal("first sync did not record b.png")
	}

	// b.png disappears from the manifest: its entry is in scope and pruned.
	site.set("/spacemap/xml/resources.xml", []byte(fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<filecollection>
  <location id="gfx" path="graphics/"/>
  <file id="a" location="gfx" name="a" type="png" hash="%s"/>
</filecollection>`, manifestHash(assetA))))
	res = runCLI(t, siteArgs("sync", out, site.URL, "--category", "spacemap")...)
	if res.code != ExitOK {
		t.Fatalf("second sync exit %d\n%s\n%s", res.code, res.stdout, res.stderr)
	}
	st := loadState(t, out)
	if _, ok := st.Resources["spacemap/graphics/b.png"]; ok {
		t.Errorf("state entry for removed b.png was kept: %v", st.Resources)
	}
	if _, ok := st.Resources["spacemap/graphics/a.png"]; !ok {
		t.Errorf("state entry for a.png was dropped: %v", st.Resources)
	}
}

func TestStateScope(t *testing.T) {
	cfg := config{
		Categories: map[string]bool{"do_img": true},
		Languages:  map[string]bool{"en": true},
	}
	scope := stateScope(cfg)
	cases := []struct {
		rel   string
		entry state.StateEntry
		want  bool
	}{
		{"do_img/en/a.png", state.StateEntry{Category: "do_img"}, true},
		{"do_img/global/a.png", state.StateEntry{Category: "do_img"}, true},
		{"do_img/de/a.png", state.StateEntry{Category: "do_img"}, false},
		{"spacemap/graphics/a.png", state.StateEntry{Category: "spacemap"}, false},
		// Legacy entries without a category are never dropped by a category filter.
		{"do_img/en/legacy.png", state.StateEntry{}, false},
	}
	for _, tc := range cases {
		if got := scope(tc.rel, tc.entry); got != tc.want {
			t.Errorf("scope(%q, %+v) = %v, want %v", tc.rel, tc.entry, got, tc.want)
		}
	}
	all := stateScope(config{Categories: map[string]bool{"all": true}})
	if !all("whatever/legacy.bin", state.StateEntry{}) {
		t.Error("with every category selected, a legacy entry should be in scope")
	}
}

type runResult struct {
	code   int
	stdout string
	stderr string
}

func runCLI(t *testing.T, args ...string) runResult {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), args, &stdout, &stderr)
	return runResult{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func siteArgs(command, out, baseURL string, extra ...string) []string {
	args := []string{command, "--output", out, "--base-url", baseURL, "--request-interval", "0s", "--concurrency", "4", "--no-color"}
	return append(args, extra...)
}

func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected %s to exist: %v", path, err)
	}
}

func TestCommandsEndToEnd(t *testing.T) {
	site := newFakeSite(t)
	root := t.TempDir()
	out := filepath.Join(root, "mirror")

	// First sync downloads everything.
	res := runCLI(t, siteArgs("sync", out, site.URL)...)
	if res.code != ExitOK {
		t.Fatalf("first sync exit %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	for _, rel := range []string{"spacemap/graphics/a.png", "spacemap/graphics/b.png", "spacemap/main.swf", "crossdomain.xml", state.FileName, "app.log"} {
		mustExist(t, filepath.Join(out, filepath.FromSlash(rel)))
	}
	if got, _ := os.ReadFile(filepath.Join(out, "spacemap", "graphics", "a.png")); !bytes.Equal(got, assetA) {
		t.Fatalf("a.png content = %q", got)
	}
	if !strings.Contains(res.stdout, "resource_localized.xml not found") {
		t.Errorf("expected a warning for the missing language bootstrap seed, stdout:\n%s", res.stdout)
	}
	if !strings.Contains(res.stdout, "skipped 1 invalid or unsafe entries") {
		t.Errorf("expected a warning for the ../ manifest entry, stdout:\n%s", res.stdout)
	}
	// Nothing may be written outside the output directory.
	if _, err := os.Stat(filepath.Join(root, "evil.txt")); !os.IsNotExist(err) {
		t.Fatalf("evil.txt escaped the output directory (err=%v)", err)
	}
	if n := site.getCount("/evil.txt"); n != 0 {
		t.Fatalf("unsafe entry was requested %d times", n)
	}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Name() == "evil.txt" {
			t.Errorf("unexpected file %s", path)
		}
		return nil
	})
	logData, err := os.ReadFile(filepath.Join(out, "app.log"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(logData, []byte("\x1b")) {
		t.Error("log file contains ANSI escape sequences")
	}
	if !bytes.Contains(logData, []byte("Sync Summary")) {
		t.Error("log file is missing the sync summary")
	}

	// Second sync: everything is up to date.
	res = runCLI(t, siteArgs("sync", out, site.URL)...)
	if res.code != ExitOK {
		t.Fatalf("second sync exit %d\n%s\n%s", res.code, res.stdout, res.stderr)
	}
	if !strings.Contains(res.stdout, "No resource downloads needed.") {
		t.Fatalf("second sync planned downloads:\n%s", res.stdout)
	}
	if a, b := site.getCount("/spacemap/graphics/a.png"), site.getCount("/spacemap/graphics/b.png"); a != 1 || b != 1 {
		t.Fatalf("assets downloaded again: a=%d b=%d", a, b)
	}

	// Corrupt a file: verify reports it with exit code 3.
	aPath := filepath.Join(out, "spacemap", "graphics", "a.png")
	if err := os.WriteFile(aPath, []byte("corrupted"), 0o644); err != nil {
		t.Fatal(err)
	}
	res = runCLI(t, "verify", "--output", out, "--no-color")
	if res.code != ExitProblems {
		t.Fatalf("verify exit %d, want %d\n%s\n%s", res.code, ExitProblems, res.stdout, res.stderr)
	}
	if !strings.Contains(res.stdout, "spacemap/graphics/a.png") {
		t.Errorf("verify did not list the corrupted file:\n%s", res.stdout)
	}

	// plan reports the mismatch but downloads nothing.
	res = runCLI(t, siteArgs("plan", out, site.URL)...)
	if res.code != ExitOK {
		t.Fatalf("plan exit %d\n%s\n%s", res.code, res.stdout, res.stderr)
	}
	if !strings.Contains(res.stdout, "DOWNLOAD spacemap/graphics/a.png (hash-mismatch)") {
		t.Errorf("plan did not list the mismatch:\n%s", res.stdout)
	}
	if got, _ := os.ReadFile(aPath); string(got) != "corrupted" {
		t.Fatal("plan modified a.png")
	}
	if n := site.getCount("/spacemap/graphics/a.png"); n != 1 {
		t.Fatalf("plan downloaded a.png (%d GETs)", n)
	}

	// A corrupt state file is a warning; sync still repairs the mirror.
	if err := os.WriteFile(filepath.Join(out, state.FileName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	res = runCLI(t, siteArgs("sync", out, site.URL)...)
	if res.code != ExitOK {
		t.Fatalf("sync with corrupt state exit %d\n%s\n%s", res.code, res.stdout, res.stderr)
	}
	if !strings.Contains(res.stdout, "WARN") || !strings.Contains(res.stdout, "corrupt") {
		t.Errorf("expected a corrupt-state warning:\n%s", res.stdout)
	}
	if got, _ := os.ReadFile(aPath); !bytes.Equal(got, assetA) {
		t.Fatalf("a.png not repaired: %q", got)
	}
	if _, err := state.Load(out); err != nil {
		t.Fatalf("state not rewritten: %v", err)
	}

	res = runCLI(t, "verify", "--output", out, "--no-color")
	if res.code != ExitOK {
		t.Fatalf("final verify exit %d\n%s\n%s", res.code, res.stdout, res.stderr)
	}
}

func TestSyncFailsWhenRequiredSeedMissing(t *testing.T) {
	site := newFakeSite(t)
	site.remove("/spacemap/main.swf")
	out := filepath.Join(t.TempDir(), "mirror")
	res := runCLI(t, siteArgs("sync", out, site.URL)...)
	if res.code != ExitFailure {
		t.Fatalf("exit %d, want %d\n%s\n%s", res.code, ExitFailure, res.stdout, res.stderr)
	}
	if !strings.Contains(res.stderr, "spacemap/main.swf") {
		t.Errorf("stderr does not name the missing seed:\n%s", res.stderr)
	}
}

func TestSyncInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := filepath.Join(t.TempDir(), "mirror")
	var stdout, stderr bytes.Buffer
	code := Run(ctx, []string{"sync", "--output", out, "--base-url", "http://127.0.0.1:1", "--no-color"}, &stdout, &stderr)
	if code != ExitInterrupted {
		t.Fatalf("exit %d, want %d\n%s\n%s", code, ExitInterrupted, stdout.String(), stderr.String())
	}
}

func TestUnknownCommandCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	for _, args := range [][]string{{"bogus"}, {"bogus", "--output", "x"}, {"--output", "x"}} {
		res := runCLI(t, args...)
		if res.code != ExitUsage {
			t.Errorf("%v: exit %d, want %d", args, res.code, ExitUsage)
		}
		if res.stdout != "" {
			t.Errorf("%v: unexpected stdout %q", args, res.stdout)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("unknown command created %d entries, e.g. %s", len(entries), entries[0].Name())
	}
}

func TestHelpVersionAndUsage(t *testing.T) {
	res := runCLI(t, "--version")
	if res.code != ExitOK || res.stdout != versionLine()+"\n" {
		t.Errorf("--version: exit %d, stdout %q", res.code, res.stdout)
	}
	if !strings.HasPrefix(res.stdout, "darkorbit-downloader ") {
		t.Errorf("unexpected version line %q", res.stdout)
	}
	res = runCLI(t, "version")
	if res.code != ExitOK || res.stdout != versionLine()+"\n" {
		t.Errorf("version: exit %d, stdout %q", res.code, res.stdout)
	}
	for _, arg := range []string{"help", "-h", "--help"} {
		res = runCLI(t, arg)
		if res.code != ExitOK || !strings.Contains(res.stdout, "Usage:") {
			t.Errorf("%s: exit %d, stdout %q", arg, res.code, res.stdout)
		}
	}
	res = runCLI(t, "verify", "-h")
	if res.code != ExitOK || !strings.Contains(res.stdout, "Usage:") {
		t.Errorf("verify -h: exit %d, stdout %q", res.code, res.stdout)
	}
	res = runCLI(t)
	if res.code != ExitUsage || !strings.Contains(res.stderr, "Usage:") || res.stdout != "" {
		t.Errorf("no args: exit %d, stdout %q", res.code, res.stdout)
	}
	res = runCLI(t, "-v")
	if res.code != ExitUsage {
		t.Errorf("-v: exit %d, want %d", res.code, ExitUsage)
	}
	res = runCLI(t, "sync", "--bogus-flag")
	if res.code != ExitUsage || !strings.Contains(res.stderr, "bogus-flag") || !strings.Contains(res.stderr, "Usage:") {
		t.Errorf("bad flag: exit %d, stderr %q", res.code, res.stderr)
	}
	if strings.Contains(res.stderr, "-base-url string") {
		t.Error("bad flag printed Go's default flag usage")
	}
}

func TestVerifyOutputNamedVersion(t *testing.T) {
	out := filepath.Join(t.TempDir(), "version")
	res := runCLI(t, "verify", "--output", out, "--no-color")
	if strings.Contains(res.stdout, versionLine()) {
		t.Fatalf("verify --output version printed the version:\n%s", res.stdout)
	}
	if res.code != ExitProblems {
		t.Fatalf("verify of an empty mirror: exit %d, want %d\n%s\n%s", res.code, ExitProblems, res.stdout, res.stderr)
	}
}

func TestParseConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "defaults", args: nil},
		{name: "explicit valid", args: []string{"--concurrency", "4", "--min-concurrency", "4", "--request-interval", "0s", "--category", "spacemap,core", "--base-url", "http://localhost:8080/"}},
		{name: "zero concurrency", args: []string{"--concurrency", "0"}, wantErr: "--concurrency"},
		{name: "min above max", args: []string{"--concurrency", "2", "--min-concurrency", "3"}, wantErr: "--min-concurrency"},
		{name: "min zero", args: []string{"--min-concurrency", "0"}, wantErr: "--min-concurrency"},
		{name: "negative interval", args: []string{"--request-interval", "-1s"}, wantErr: "--request-interval"},
		{name: "ftp scheme", args: []string{"--base-url", "ftp://example.com"}, wantErr: "scheme"},
		{name: "no scheme", args: []string{"--base-url", "www.darkorbit.com"}, wantErr: "scheme"},
		{name: "no host", args: []string{"--base-url", "https://"}, wantErr: "host"},
		{name: "unknown category", args: []string{"--category", "spacemap,bogus"}, wantErr: "valid: all, spacemap"},
		{name: "stray positional", args: []string{"--force", "extra"}, wantErr: `unexpected argument "extra"`},
		{name: "empty output", args: []string{"--output", ""}, wantErr: "--output"},
		{name: "unknown flag", args: []string{"--nope"}, wantErr: "nope"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseConfig("sync", tt.args)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseConfigLogFile(t *testing.T) {
	cfg, err := parseConfig("sync", []string{"--output", "out"})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("out", "app.log"); cfg.LogFile != want {
		t.Errorf("default log file = %q, want %q", cfg.LogFile, want)
	}
	cfg, err = parseConfig("sync", []string{"--log-file", ""})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogFile != "" {
		t.Errorf("--log-file \"\" = %q, want disabled", cfg.LogFile)
	}
	cfg, err = parseConfig("sync", []string{"--base-url", "https://example.com///"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != "https://example.com" {
		t.Errorf("BaseURL = %q", cfg.BaseURL)
	}
}

func noWarn(t *testing.T) func(string, ...any) {
	return func(format string, args ...any) {
		t.Errorf("unexpected warning: "+format, args...)
	}
}

func TestBuildDownloadPlan(t *testing.T) {
	dir := t.TempDir()

	matchPath := filepath.Join(dir, "spacemap", "graphics", "match.swf")
	mismatchPath := filepath.Join(dir, "spacemap", "graphics", "mismatch.swf")
	if err := os.MkdirAll(filepath.Dir(matchPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(matchPath, []byte("expected"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mismatchPath, []byte("old-data"), 0o644); err != nil {
		t.Fatal(err)
	}

	resources := []model.Resource{
		{RelativePath: "spacemap/graphics/missing.swf", URL: "https://example.com/missing.swf", Hash: manifestHash([]byte("expected")), Category: "spacemap"},
		{RelativePath: "spacemap/graphics/match.swf", URL: "https://example.com/match.swf", Hash: manifestHash([]byte("expected")), Category: "spacemap"},
		{RelativePath: "spacemap/graphics/mismatch.swf", URL: "https://example.com/mismatch.swf", Hash: manifestHash([]byte("new-data")), Category: "spacemap"},
	}

	plan, skipped, st, err := BuildDownloadPlan(context.Background(), dir, resources, nil, false, noWarn(t))
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 1 {
		t.Fatalf("expected 1 skipped resource, got %d", skipped)
	}
	reasons := map[string]string{}
	for _, item := range plan {
		reasons[item.Resource.RelativePath] = item.Reason
	}
	if len(plan) != 2 || reasons["spacemap/graphics/missing.swf"] != "missing" || reasons["spacemap/graphics/mismatch.swf"] != "hash-mismatch" {
		t.Fatalf("unexpected plan: %v", reasons)
	}
	entry, ok := st.Resources["spacemap/graphics/match.swf"]
	if !ok || entry.Size != int64(len("expected")) || entry.ModTime == 0 {
		t.Fatalf("state entry for match.swf = %+v (present %v)", entry, ok)
	}
	if len(st.Resources) != 1 {
		t.Fatalf("state should only hold up-to-date resources, got %v", st.Resources)
	}

	plan, _, _, err = BuildDownloadPlan(context.Background(), dir, resources, nil, true, noWarn(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range plan {
		if item.Reason != "forced" {
			t.Fatalf("--force planned %s with reason %q", item.Resource.RelativePath, item.Reason)
		}
	}
	if len(plan) != 3 {
		t.Fatalf("--force planned %d items, want 3", len(plan))
	}
}

func TestBuildDownloadPlanTrustsMatchingState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.bin")
	if err := os.WriteFile(path, []byte("abcd"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// The manifest hash does not match the content, but the state entry
	// says the file was verified with this size and mtime: no re-hash.
	resource := model.Resource{RelativePath: "a.bin", URL: "u", Hash: manifestHash([]byte("other"))}
	st := &state.State{Resources: map[string]state.StateEntry{
		"a.bin":      {Hash: strings.ToUpper(resource.Hash), URL: "u", Size: info.Size(), ModTime: info.ModTime().UnixNano()},
		"stale.file": {Hash: "x"},
	}}
	if err := state.Save(dir, st); err != nil {
		t.Fatal(err)
	}
	plan, skipped, newState, err := BuildDownloadPlan(context.Background(), dir, []model.Resource{resource}, nil, false, noWarn(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 0 || skipped != 1 {
		t.Fatalf("plan=%v skipped=%d, want fast-path skip", plan, skipped)
	}
	if _, ok := newState.Resources["stale.file"]; ok {
		t.Fatal("stale state entry was kept")
	}

	// A different mtime forces a re-hash, which detects the mismatch.
	later := info.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	plan, _, _, err = BuildDownloadPlan(context.Background(), dir, []model.Resource{resource}, nil, false, noWarn(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 1 || plan[0].Reason != "hash-mismatch" {
		t.Fatalf("plan after mtime change = %+v", plan)
	}
}

func TestBuildDownloadPlanSkipsUnsafePaths(t *testing.T) {
	dir := t.TempDir()
	var warnings []string
	warn := func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	resources := []model.Resource{{RelativePath: "../outside.bin", Hash: manifestHash([]byte("x"))}}
	plan, skipped, _, err := BuildDownloadPlan(context.Background(), dir, resources, nil, true, warn)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 0 || skipped != 0 || len(warnings) != 1 {
		t.Fatalf("plan=%v skipped=%d warnings=%v", plan, skipped, warnings)
	}
}

func TestBuildDownloadPlanCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _, err := BuildDownloadPlan(ctx, t.TempDir(), []model.Resource{{RelativePath: "a"}}, nil, false, noWarn(t))
	if err == nil {
		t.Fatal("expected a cancellation error")
	}
}

func TestIsLanguageBootstrapSeed(t *testing.T) {
	cases := map[string]bool{
		"spacemap/templates/language_en.xml":       true,
		"do_img/de/xml/resource_localized.xml":     true,
		"do_img/global/xml/resource_items.xml":     false,
		"do_img/global/xml/resource_localized.xml": false,
		"spacemap/xml/resources.xml":               false,
		"spacemap/templates/en/language_en.xml":    false,
	}
	for rel, want := range cases {
		if got := isLanguageBootstrapSeed(rel); got != want {
			t.Errorf("isLanguageBootstrapSeed(%q) = %v, want %v", rel, got, want)
		}
	}
}

func TestStripANSI(t *testing.T) {
	in := "\x1b[1;38;5;205mTitle\x1b[0m and \x1b]8;;http://x\x07link\x1b]8;;\x07"
	if got := stripANSI(in); got != "Title and link" {
		t.Fatalf("stripANSI = %q", got)
	}
}
