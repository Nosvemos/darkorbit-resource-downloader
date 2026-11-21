// Package app implements the darkorbit-downloader command line.
package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/Nosvemos/Darkorbit-Resource-Downloader/internal/discovery"
)

const defaultVersion = "dev"

// Version is set at build time with
// -ldflags "-X github.com/Nosvemos/Darkorbit-Resource-Downloader/internal/app.Version=v1.2.3".
var Version = defaultVersion

// Exit codes returned by Run.
const (
	ExitOK          = 0
	ExitFailure     = 1
	ExitUsage       = 2
	ExitProblems    = 3
	ExitInterrupted = 130
)

const (
	defaultBaseURL   = "https://www.darkorbit.com"
	defaultOutputDir = "darkorbit-files"
	defaultLogName   = "app.log"
)

// errProblems marks a run that completed but found problems: verify found
// missing or mismatched files, or some downloads failed. It maps to
// ExitProblems.
var errProblems = errors.New("problems found")

// validCategories are the values accepted by --category.
var validCategories = []string{"all", "spacemap", "do_img", "core", "templates", "unityApi", "flashAPI", "resources"}

var commands = map[string]bool{
	"sync":            true,
	"plan":            true,
	"fetch-manifests": true,
	"verify":          true,
}

type config struct {
	BaseURL             string
	OutputDir           string
	Concurrency         int
	MinConcurrency      int
	AutoTuneConcurrency bool
	RequestInterval     time.Duration
	Force               bool
	NoColor             bool
	Categories          map[string]bool
	Languages           map[string]bool
	LogFile             string
}

// Run executes the command line in args (without the program name) and
// returns the process exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr, false)
		return ExitUsage
	}

	switch args[0] {
	case "help", "-h", "--help":
		printUsage(stdout, false)
		return ExitOK
	case "version", "--version":
		_, _ = fmt.Fprintln(stdout, versionLine())
		return ExitOK
	}

	command := args[0]
	if !commands[command] {
		if strings.HasPrefix(command, "-") {
			_, _ = fmt.Fprintf(stderr, "error: missing command before flag %q\n\n", command)
		} else {
			_, _ = fmt.Fprintf(stderr, "error: unknown command %q\n\n", command)
		}
		printUsage(stderr, false)
		return ExitUsage
	}

	cfg, err := parseConfig(command, args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage(stdout, false)
			return ExitOK
		}
		_, _ = fmt.Fprintf(stderr, "error: %v\n\n", err)
		printUsage(stderr, false)
		return ExitUsage
	}

	pr := newPrinter(stdout, stderr, cfg.NoColor)
	defer pr.Close()

	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		pr.Errorf("create output directory: %v", err)
		return ExitFailure
	}
	if cfg.LogFile != "" {
		if err := pr.openLog(cfg.LogFile); err != nil {
			pr.Errorf("open log file: %v", err)
			return ExitFailure
		}
		pr.log(fmt.Sprintf("=== %s %s, %s ===\n", time.Now().Format(time.RFC3339), command, versionLine()))
	}

	switch command {
	case "sync":
		err = runSync(ctx, cfg, pr)
	case "plan":
		err = runPlan(ctx, cfg, pr)
	case "fetch-manifests":
		err = runFetchManifests(ctx, cfg, pr)
	case "verify":
		err = runVerify(ctx, cfg, pr)
	}

	switch {
	case err == nil:
		return ExitOK
	case ctx.Err() != nil || errors.Is(err, context.Canceled):
		pr.Errorf("interrupted")
		return ExitInterrupted
	case errors.Is(err, errProblems):
		pr.Errorf("%v", err)
		return ExitProblems
	default:
		pr.Errorf("%v", err)
		return ExitFailure
	}
}

func parseConfig(command string, args []string) (config, error) {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	// Run reports errors together with the styled usage text, so the flag
	// package must not print its own.
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}

	baseURL := fs.String("base-url", defaultBaseURL, "")
	outputDir := fs.String("output", defaultOutputDir, "")
	concurrency := fs.Int("concurrency", 8, "")
	minConcurrency := fs.Int("min-concurrency", 1, "")
	autoTune := fs.Bool("auto-tune-concurrency", true, "")
	requestInterval := fs.Duration("request-interval", 250*time.Millisecond, "")
	force := fs.Bool("force", false, "")
	noColor := fs.Bool("no-color", false, "")
	categoryCSV := fs.String("category", "all", "")
	languageCSV := fs.String("languages", "en", "")
	logFile := fs.String("log-file", "", "")

	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if fs.NArg() > 0 {
		return config{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	logSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "log-file" {
			logSet = true
		}
	})

	if strings.TrimSpace(*outputDir) == "" {
		return config{}, errors.New("--output must not be empty")
	}
	if *concurrency < 1 {
		return config{}, fmt.Errorf("--concurrency must be at least 1, got %d", *concurrency)
	}
	if *minConcurrency < 1 || *minConcurrency > *concurrency {
		return config{}, fmt.Errorf("--min-concurrency must be between 1 and --concurrency (%d), got %d", *concurrency, *minConcurrency)
	}
	if *requestInterval < 0 {
		return config{}, fmt.Errorf("--request-interval must not be negative, got %s", *requestInterval)
	}
	base, err := validateBaseURL(*baseURL)
	if err != nil {
		return config{}, err
	}
	categories, err := parseCategories(*categoryCSV)
	if err != nil {
		return config{}, err
	}

	cfg := config{
		BaseURL:             base,
		OutputDir:           *outputDir,
		Concurrency:         *concurrency,
		MinConcurrency:      *minConcurrency,
		AutoTuneConcurrency: *autoTune,
		RequestInterval:     *requestInterval,
		Force:               *force,
		NoColor:             *noColor,
		Categories:          categories,
		Languages:           parseLanguages(*languageCSV),
		LogFile:             *logFile,
	}
	if !logSet {
		cfg.LogFile = filepath.Join(cfg.OutputDir, defaultLogName)
	}
	return cfg, nil
}

func validateBaseURL(raw string) (string, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("invalid --base-url %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("invalid --base-url %q: scheme must be http or https", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("invalid --base-url %q: missing host", raw)
	}
	return trimmed, nil
}

func parseCategories(csv string) (map[string]bool, error) {
	valid := map[string]bool{}
	for _, c := range validCategories {
		valid[c] = true
	}
	result := map[string]bool{}
	for _, part := range strings.Split(csv, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !valid[part] {
			return nil, fmt.Errorf("unknown category %q (valid: %s)", part, strings.Join(validCategories, ", "))
		}
		result[part] = true
	}
	if len(result) == 0 {
		result["all"] = true
	}
	return result, nil
}

func parseLanguages(csv string) map[string]bool {
	result := map[string]bool{}
	for _, part := range strings.Split(csv, ",") {
		part = discovery.CanonicalLanguageCode(part)
		if part == "" {
			continue
		}
		result[part] = true
	}
	if len(result) == 0 {
		result["en"] = true
	}
	return result
}

// versionLine is the single line printed by "version" / "--version".
func versionLine() string {
	return fmt.Sprintf("darkorbit-downloader %s (%s/%s, %s)", resolvedVersion(), runtime.GOOS, runtime.GOARCH, runtime.Version())
}

// resolvedVersion returns Version, or, when it was not set at build time, the
// module version or VCS revision recorded by the Go toolchain.
func resolvedVersion() string {
	if Version != defaultVersion {
		return Version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return Version
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	var revision string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if revision == "" {
		return Version
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	v := Version + "-" + revision
	if dirty {
		v += "-dirty"
	}
	return v
}

// binaryName is the program name as invoked, without directory or ".exe".
func binaryName() string {
	name := filepath.Base(os.Args[0])
	if ext := filepath.Ext(name); strings.EqualFold(ext, ".exe") {
		name = strings.TrimSuffix(name, ext)
	}
	if name == "" || name == "." || name == string(filepath.Separator) {
		return "darkorbit-downloader"
	}
	return name
}

func titleCase(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}

func printUsage(w io.Writer, noColor bool) {
	r := lipgloss.NewRenderer(w)
	if noColor {
		r.SetColorProfile(termenv.Ascii)
	}
	title := r.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))
	section := r.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	name := r.NewStyle().Bold(true).Foreground(lipgloss.Color("42"))
	desc := r.NewStyle().Foreground(lipgloss.Color("250"))

	var b strings.Builder
	line := func(n, d string) {
		fmt.Fprintf(&b, "  %s %s\n", name.Render(fmt.Sprintf("%-25s", n)), desc.Render(d))
	}

	fmt.Fprintf(&b, "%s\n\n", title.Render(fmt.Sprintf("Usage: %s <command> [flags]", binaryName())))

	fmt.Fprintln(&b, section.Render("COMMANDS:"))
	line("sync", "Fetch manifests and core files, then download missing or changed resources")
	line("plan", "Fetch manifests and show what sync would download, without downloading assets")
	line("fetch-manifests", "Refresh manifests, language files and templates only")
	line("verify", "Check local files against the local manifests (offline unless --languages=all)")
	line("version", "Print version information (also --version)")
	line("help", "Show this help (also -h, --help)")
	b.WriteString("\n")

	fmt.Fprintln(&b, section.Render("GENERAL OPTIONS:"))
	line("--base-url URL", fmt.Sprintf("Server to mirror, http or https (default %q)", defaultBaseURL))
	line("--output DIR", fmt.Sprintf("Mirror directory, also holds .app-state.json (default %q)", defaultOutputDir))
	line("--force", "Re-download every selected file (default false)")
	line("--log-file PATH", fmt.Sprintf("Plain-text log file, empty disables it (default <output>/%s)", defaultLogName))
	line("--no-color", "Disable colored output (NO_COLOR=1 in the environment also works)")
	b.WriteString("\n")

	fmt.Fprintln(&b, section.Render("CONCURRENCY & PACING:"))
	line("--concurrency N", "Parallel downloads, at least 1 (default 8)")
	line("--min-concurrency N", "Lowest concurrency the auto-tuner may drop to, 1..concurrency (default 1)")
	line("--auto-tune-concurrency", "Lower concurrency on 429/503 responses and raise it again (default true)")
	line("--request-interval D", "Minimum delay between starting requests, e.g. 250ms or 1s (default 250ms)")
	b.WriteString("\n")

	fmt.Fprintln(&b, section.Render("FILTERS:"))
	line("--category LIST", fmt.Sprintf("Comma-separated: %s (default \"all\")", strings.Join(validCategories, ", ")))
	line("--languages LIST", "Comma-separated language codes, e.g. en,de,tr, or \"all\" (default \"en\")")
	b.WriteString("\n")

	fmt.Fprintln(&b, section.Render("EXIT CODES:"))
	fmt.Fprintln(&b, desc.Render("  0 success, 1 runtime error, 2 usage error, 3 verify found problems or downloads failed, 130 interrupted"))

	_, _ = io.WriteString(w, b.String())
}

func matchesCategory(category string, allowed map[string]bool) bool {
	if len(allowed) == 0 || allowed["all"] {
		return true
	}
	return allowed[category]
}
