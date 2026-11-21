package downloader

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Nosvemos/Darkorbit-Resource-Downloader/internal/integrity"
	"github.com/Nosvemos/Darkorbit-Resource-Downloader/internal/model"
	"github.com/Nosvemos/Darkorbit-Resource-Downloader/internal/safepath"
)

const (
	maxAttempts = 6
	// maxHashMismatches is how many hash mismatches a single download may hit
	// before giving up (i.e. one retry after the first mismatch).
	maxHashMismatches = 2
	// maxDrainBytes bounds how much of a non-2xx body is read so the
	// connection can be reused.
	maxDrainBytes = 64 << 10

	defaultStallTimeout   = 30 * time.Second
	defaultRetryBaseDelay = time.Second
	defaultRetryMaxDelay  = 15 * time.Second
	defaultMaxCooldown    = 20 * time.Second

	partSuffix = ".part"
)

// ErrHashMismatch is matched (via errors.Is) by *HashMismatchError.
var ErrHashMismatch = errors.New("downloaded content does not match manifest hash")

// ErrStalled is returned (wrapped) when a response body delivers no bytes for
// longer than Config.StallTimeout.
var ErrStalled = errors.New("download stalled")

type Result struct {
	Item model.DownloadItem
	Err  error
}

type Stats struct {
	EffectiveConcurrency int
	MaxConcurrency       int
	MinConcurrency       int
	InFlight             int
	SuccessStreak        int
	AutoTuneEnabled      bool
}

type Client struct {
	httpClient     *http.Client
	pacer          *adaptivePacer
	limiter        *adaptiveConcurrency
	stallTimeout   time.Duration
	retryBaseDelay time.Duration
	retryMaxDelay  time.Duration
}

type HTTPStatusError struct {
	StatusCode int
	URL        string
	RetryAfter time.Duration
}

// HashMismatchError reports a download whose MD5 does not match the expected
// manifest hash. errors.Is(err, ErrHashMismatch) reports true for it.
type HashMismatchError struct {
	URL      string
	Expected string
	Actual   string
}

type Config struct {
	// StallTimeout cancels a request when its response body delivers no bytes
	// for this long. There is no limit on total transfer time. Default 30s.
	StallTimeout    time.Duration
	RequestInterval time.Duration
	MaxCooldown     time.Duration
	MaxConcurrency  int
	MinConcurrency  int
	// RetryBaseDelay is the first retry backoff; it doubles per attempt up to
	// RetryMaxDelay. Server Retry-After values take precedence. Defaults 1s/15s.
	RetryBaseDelay      time.Duration
	RetryMaxDelay       time.Duration
	AutoTuneConcurrency bool
}

// nonRetryableError marks failures that retrying cannot fix, such as local
// filesystem errors and malformed requests.
type nonRetryableError struct {
	err error
}

type adaptivePacer struct {
	minInterval time.Duration
	maxCooldown time.Duration

	mu          sync.Mutex
	nextRequest time.Time
	cooldownEnd time.Time
	penalty     int
}

type adaptiveConcurrency struct {
	enabled bool
	max     int
	min     int

	mu            sync.Mutex
	inFlight      int
	current       int
	successStreak int
	// changed is closed and replaced whenever a slot may have become
	// available, waking every waiter in Acquire.
	changed chan struct{}
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("unexpected status %d for %s", e.StatusCode, e.URL)
}

func (e *HashMismatchError) Error() string {
	return fmt.Sprintf("hash mismatch for %s: expected %s, got %s", e.URL, e.Expected, e.Actual)
}

func (e *HashMismatchError) Is(target error) bool {
	return target == ErrHashMismatch
}

func (e *nonRetryableError) Error() string { return e.err.Error() }
func (e *nonRetryableError) Unwrap() error { return e.err }

func New() *Client {
	return NewWithConfig(Config{
		StallTimeout:        defaultStallTimeout,
		RequestInterval:     250 * time.Millisecond,
		MaxCooldown:         defaultMaxCooldown,
		MaxConcurrency:      8,
		MinConcurrency:      1,
		AutoTuneConcurrency: true,
	})
}

func NewWithConfig(cfg Config) *Client {
	if cfg.StallTimeout <= 0 {
		cfg.StallTimeout = defaultStallTimeout
	}
	if cfg.RequestInterval < 0 {
		cfg.RequestInterval = 0
	}
	if cfg.MaxCooldown <= 0 {
		cfg.MaxCooldown = defaultMaxCooldown
	}
	if cfg.MaxConcurrency < 1 {
		cfg.MaxConcurrency = 1
	}
	if cfg.MinConcurrency < 1 {
		cfg.MinConcurrency = 1
	}
	if cfg.MinConcurrency > cfg.MaxConcurrency {
		cfg.MinConcurrency = cfg.MaxConcurrency
	}
	if cfg.RetryBaseDelay <= 0 {
		cfg.RetryBaseDelay = defaultRetryBaseDelay
	}
	if cfg.RetryMaxDelay <= 0 {
		cfg.RetryMaxDelay = defaultRetryMaxDelay
	}
	if cfg.RetryMaxDelay < cfg.RetryBaseDelay {
		cfg.RetryMaxDelay = cfg.RetryBaseDelay
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   cfg.MaxConcurrency,
		MaxConnsPerHost:       cfg.MaxConcurrency,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: time.Second,
	}

	return &Client{
		httpClient:     &http.Client{Transport: transport},
		pacer:          newAdaptivePacer(cfg.RequestInterval, cfg.MaxCooldown),
		limiter:        newAdaptiveConcurrency(cfg.MaxConcurrency, cfg.MinConcurrency, cfg.AutoTuneConcurrency),
		stallTimeout:   cfg.StallTimeout,
		retryBaseDelay: cfg.RetryBaseDelay,
		retryMaxDelay:  cfg.RetryMaxDelay,
	}
}

// FetchToFile downloads url to destination, retrying transient failures. The
// content is not hash-verified.
func (c *Client) FetchToFile(ctx context.Context, url, destination string) error {
	return c.fetchToFileWithRetry(ctx, url, destination, "")
}

// DownloadAll downloads items under outputDir using concurrency workers. Each
// item's destination is resolved with safepath.Join, so an unsafe relative
// path fails that item without touching the filesystem. Items with a non-empty
// Resource.Hash are verified against it before being moved into place.
func (c *Client) DownloadAll(ctx context.Context, outputDir string, items []model.DownloadItem, concurrency int) <-chan Result {
	results := make(chan Result)
	if concurrency < 1 {
		concurrency = 1
	}

	workCh := make(chan model.DownloadItem)
	var wg sync.WaitGroup

	worker := func() {
		defer wg.Done()
		for item := range workCh {
			dest, err := safepath.Join(outputDir, item.Resource.RelativePath)
			if err == nil {
				err = c.fetchToFileWithRetry(ctx, item.Resource.URL, dest, item.Resource.Hash)
			}
			results <- Result{Item: item, Err: err}
		}
	}

	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go worker()
	}

	go func() {
		defer close(workCh)
		for _, item := range items {
			select {
			case <-ctx.Done():
				return
			case workCh <- item:
			}
		}
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	return results
}

func (c *Client) Stats() Stats {
	return c.limiter.Stats()
}

// isTempPartName reports whether name has the shape of a temp file created by
// this package: os.CreateTemp(dir, base+".*.part") replaces the "*" with a
// decimal uint32 (1 to 10 digits), giving "<base>.<digits>.part". Any other
// name ending in ".part" (e.g. "notes.part" or "video.mp4.part") is not ours.
func isTempPartName(name string) bool {
	stem, ok := strings.CutSuffix(name, partSuffix)
	if !ok {
		return false
	}
	dot := strings.LastIndexByte(stem, '.')
	if dot <= 0 {
		return false
	}
	digits := stem[dot+1:]
	if len(digits) == 0 || len(digits) > 10 {
		return false
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return false
		}
	}
	return true
}

// RemoveStaleParts deletes leftover download temp files anywhere under root,
// e.g. from an interrupted previous run. Only names matching this package's
// own temp pattern "<base>.<digits>.part" are removed, so unrelated files that
// merely end in ".part" survive even when root is a shared folder. It returns
// how many were removed. A missing root is not an error.
func RemoveStaleParts(root string) (int, error) {
	removed := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.Type().IsRegular() || !isTempPartName(d.Name()) {
			return nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		removed++
		return nil
	})
	return removed, err
}

func (c *Client) fetchToFileWithRetry(ctx context.Context, url, destination, expectedHash string) error {
	var lastErr error
	mismatches := 0
	for i := 0; i < maxAttempts; i++ {
		if i > 0 {
			timer := time.NewTimer(c.retryDelay(lastErr, i))
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}

		lastErr = c.attempt(ctx, url, destination, expectedHash)
		if lastErr == nil {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			if errors.Is(lastErr, ctxErr) {
				return lastErr
			}
			return ctxErr
		}
		if errors.Is(lastErr, ErrHashMismatch) {
			mismatches++
			if mismatches >= maxHashMismatches {
				return lastErr
			}
			continue
		}
		if !isRetryableError(lastErr) {
			return lastErr
		}
	}
	return lastErr
}

// attempt performs one request while holding a concurrency slot. The slot is
// released before the caller sleeps for backoff.
func (c *Client) attempt(ctx context.Context, url, destination, expectedHash string) error {
	if err := c.limiter.Acquire(ctx); err != nil {
		return err
	}
	defer c.limiter.Release()

	if err := c.pacer.Wait(ctx); err != nil {
		return err
	}
	err := c.fetchToFile(ctx, url, destination, expectedHash)
	if err == nil {
		c.pacer.NoteSuccess()
		c.limiter.NoteSuccess()
		return nil
	}
	if isThrottleError(err) {
		c.pacer.NoteThrottle(err)
		c.limiter.NoteThrottle()
	}
	return err
}

func (c *Client) fetchToFile(ctx context.Context, url, destination, expectedHash string) error {
	reqCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return &nonRetryableError{err: err}
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "*/*")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	body := newStallReader(resp.Body, c.stallTimeout, func() { cancel(ErrStalled) })
	defer func() {
		body.stop()
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.CopyN(io.Discard, body, maxDrainBytes)
		return &HTTPStatusError{
			StatusCode: resp.StatusCode,
			URL:        url,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}

	dir := filepath.Dir(destination)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return &nonRetryableError{err: err}
	}

	file, err := os.CreateTemp(dir, filepath.Base(destination)+".*"+partSuffix)
	if err != nil {
		return &nonRetryableError{err: err}
	}
	tmpPath := file.Name()
	closed, committed := false, false
	defer func() {
		if !closed {
			_ = file.Close()
		}
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()

	hasher := md5.New()
	if _, err := io.Copy(io.MultiWriter(fileWriter{file}, hasher), body); err != nil {
		if errors.Is(context.Cause(reqCtx), ErrStalled) {
			return fmt.Errorf("%w: no data received for %s from %s", ErrStalled, c.stallTimeout, url)
		}
		return err
	}
	if err := file.Chmod(0o644); err != nil {
		return &nonRetryableError{err: err}
	}
	if err := file.Sync(); err != nil {
		return &nonRetryableError{err: err}
	}
	closed = true
	if err := file.Close(); err != nil {
		return &nonRetryableError{err: err}
	}

	if expectedHash != "" {
		actual := hex.EncodeToString(hasher.Sum(nil))
		if !integrity.MatchesManifestHash(actual, expectedHash) {
			return &HashMismatchError{URL: url, Expected: expectedHash, Actual: actual}
		}
	}

	if err := os.Rename(tmpPath, destination); err != nil {
		return &nonRetryableError{err: err}
	}
	committed = true
	return nil
}

// fileWriter tags write errors as non-retryable so io.Copy failures on the
// local side are distinguishable from network read failures.
type fileWriter struct {
	f *os.File
}

func (w fileWriter) Write(p []byte) (int, error) {
	n, err := w.f.Write(p)
	if err != nil {
		err = &nonRetryableError{err: err}
	}
	return n, err
}

// stallReader invokes onStall when no bytes have been read for timeout.
type stallReader struct {
	r       io.Reader
	timeout time.Duration
	timer   *time.Timer
}

func newStallReader(r io.Reader, timeout time.Duration, onStall func()) *stallReader {
	return &stallReader{r: r, timeout: timeout, timer: time.AfterFunc(timeout, onStall)}
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.timer.Reset(s.timeout)
	}
	return n, err
}

func (s *stallReader) stop() {
	s.timer.Stop()
}

// isRetryableError classifies a failed attempt. Local filesystem and request
// construction errors, cancellation, hash mismatches (handled separately by
// the retry loop) and 4xx responses other than 408/429 are permanent; network
// errors, stalls, truncated bodies, 5xx, 408 and 429 are retried.
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, ErrHashMismatch) {
		return false
	}
	var permanent *nonRetryableError
	if errors.As(err, &permanent) {
		return false
	}
	var statusErr *HTTPStatusError
	if errors.As(err, &statusErr) {
		switch statusErr.StatusCode {
		case http.StatusRequestTimeout, http.StatusTooManyRequests:
			return true
		default:
			return statusErr.StatusCode >= 500
		}
	}
	return true
}

func (c *Client) retryDelay(err error, attempt int) time.Duration {
	var statusErr *HTTPStatusError
	if errors.As(err, &statusErr) && statusErr.RetryAfter > 0 {
		return statusErr.RetryAfter
	}

	if attempt < 1 {
		attempt = 1
	}
	delay := c.retryBaseDelay << min(attempt-1, 4)
	return min(delay, c.retryMaxDelay)
}

func isThrottleError(err error) bool {
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) {
		return false
	}
	return statusErr.StatusCode == http.StatusTooManyRequests || statusErr.StatusCode == http.StatusServiceUnavailable
}

func parseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}

	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds < 1 {
			return time.Second
		}
		if seconds > 30 {
			return 30 * time.Second
		}
		return time.Duration(seconds) * time.Second
	}

	when, err := http.ParseTime(value)
	if err != nil {
		return 0
	}
	delay := time.Until(when)
	if delay <= 0 {
		return time.Second
	}
	if delay > 30*time.Second {
		return 30 * time.Second
	}
	return delay
}

func newAdaptivePacer(minInterval, maxCooldown time.Duration) *adaptivePacer {
	return &adaptivePacer{
		minInterval: minInterval,
		maxCooldown: maxCooldown,
	}
}

func (p *adaptivePacer) Wait(ctx context.Context) error {
	p.mu.Lock()
	target := time.Now()
	target = maxTime(target, p.cooldownEnd)
	target = maxTime(target, p.nextRequest)
	if p.minInterval > 0 {
		p.nextRequest = target.Add(p.minInterval)
	} else {
		p.nextRequest = target
	}
	p.mu.Unlock()

	delay := time.Until(target)
	if delay <= 0 {
		return nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (p *adaptivePacer) NoteSuccess() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.penalty > 0 {
		p.penalty--
	}
	if p.penalty == 0 && time.Now().After(p.cooldownEnd) {
		p.cooldownEnd = time.Time{}
	}
}

func (p *adaptivePacer) NoteThrottle(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.penalty++
	if p.penalty > 6 {
		p.penalty = 6
	}

	delay := p.penaltyDelay(err)
	if delay > p.maxCooldown {
		delay = p.maxCooldown
	}

	cooldown := time.Now().Add(delay)
	if cooldown.After(p.cooldownEnd) {
		p.cooldownEnd = cooldown
	}
	if p.nextRequest.Before(p.cooldownEnd) {
		p.nextRequest = p.cooldownEnd
	}
}

func (p *adaptivePacer) penaltyDelay(err error) time.Duration {
	var statusErr *HTTPStatusError
	if errors.As(err, &statusErr) && statusErr.RetryAfter > 0 {
		return max(statusErr.RetryAfter, p.minInterval)
	}

	if p.penalty < 1 {
		return max(time.Second, p.minInterval)
	}
	delay := time.Duration(1<<min(p.penalty-1, 4)) * time.Second
	return max(delay, p.minInterval)
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func newAdaptiveConcurrency(maxConcurrency, minConcurrency int, enabled bool) *adaptiveConcurrency {
	return &adaptiveConcurrency{
		enabled: enabled,
		max:     maxConcurrency,
		min:     minConcurrency,
		current: maxConcurrency,
		changed: make(chan struct{}),
	}
}

func (a *adaptiveConcurrency) Acquire(ctx context.Context) error {
	for {
		a.mu.Lock()
		if a.inFlight < a.current {
			a.inFlight++
			a.mu.Unlock()
			return nil
		}
		changed := a.changed
		a.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (a *adaptiveConcurrency) Release() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.inFlight > 0 {
		a.inFlight--
		a.notifyLocked()
	}
}

func (a *adaptiveConcurrency) NoteThrottle() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.successStreak = 0
	if !a.enabled || a.current <= a.min {
		return
	}
	a.current--
}

func (a *adaptiveConcurrency) NoteSuccess() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.enabled {
		return
	}
	a.successStreak++
	threshold := max(6, a.current*3)
	if a.current < a.max && a.successStreak >= threshold {
		a.current++
		a.successStreak = 0
		a.notifyLocked()
	}
}

// notifyLocked wakes all goroutines blocked in Acquire. a.mu must be held.
func (a *adaptiveConcurrency) notifyLocked() {
	close(a.changed)
	a.changed = make(chan struct{})
}

func (a *adaptiveConcurrency) Stats() Stats {
	a.mu.Lock()
	defer a.mu.Unlock()
	return Stats{
		EffectiveConcurrency: a.current,
		MaxConcurrency:       a.max,
		MinConcurrency:       a.min,
		InFlight:             a.inFlight,
		SuccessStreak:        a.successStreak,
		AutoTuneEnabled:      a.enabled,
	}
}
