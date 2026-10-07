package health_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	health "github.com/Elagoht/collage-health"
	"github.com/Elagoht/collage/pkg/collage"
)

func ms(n int) health.Duration { return health.Duration(time.Duration(n) * time.Millisecond) }

// probe is a readiness probe and how long it took.
func probe(h http.Handler) (*httptest.ResponseRecorder, time.Duration) {
	start := time.Now()
	rec := do(h, http.MethodGet, "/readyz")
	return rec, time.Since(start)
}

// probeWithin is probe under a watchdog: a probe still blocked after limit
// fails the test at once instead of hanging until -timeout.
func probeWithin(t *testing.T, h http.Handler, limit time.Duration) (*httptest.ResponseRecorder, time.Duration) {
	t.Helper()
	type answer struct {
		rec  *httptest.ResponseRecorder
		took time.Duration
	}
	got := make(chan answer, 1)
	go func() {
		rec, took := probe(h)
		got <- answer{rec, took}
	}()
	select {
	case a := <-got:
		return a.rec, a.took
	case <-time.After(limit):
		t.Fatalf("probe still blocked after %s", limit)
		return nil, 0
	}
}

func TestCheck_FailingMakesUnavailable(t *testing.T) {
	p := health.New()
	p.Check("db", func(context.Context) error { return errors.New("dial tcp 10.0.0.5:5432: secret-host refused") })
	h := site(t, p, nil)

	rec := do(h, http.MethodGet, "/readyz")
	expect(t, rec, http.StatusServiceUnavailable, "unavailable")
	expectEndpointHeaders(t, rec)
	if strings.Contains(rec.Body.String(), "secret-host") {
		t.Fatal("the body carries the error's text")
	}
	// Liveness runs no checks.
	expect(t, do(h, http.MethodGet, "/healthz"), http.StatusOK, "ok")
}

func TestCheck_PassingIsOK(t *testing.T) {
	p := health.New()
	p.Check("db", func(context.Context) error { return nil })
	p.Check("cache", func(context.Context) error { return nil })
	expect(t, do(site(t, p, nil), http.MethodGet, "/readyz"), http.StatusOK, "ok")
}

func TestCheck_PanicIsFailing(t *testing.T) {
	p := health.New()
	p.Check("boom", func(context.Context) error { panic("kaboom") })
	h := site(t, p, nil)
	expect(t, do(h, http.MethodGet, "/readyz"), http.StatusServiceUnavailable, "unavailable")
}

func TestCheck_TimeoutIsFailing(t *testing.T) {
	release := make(chan struct{})
	var left atomic.Int32
	p := health.NewWith(health.Options{CheckTimeout: ms(50)})
	p.Check("slow", func(context.Context) error { defer left.Add(1); <-release; return nil })
	h := site(t, p, nil)
	defer close(release)

	rec, took := probeWithin(t, h, 2*time.Second)
	expect(t, rec, http.StatusServiceUnavailable, "unavailable")
	if left.Load() != 0 {
		t.Fatal("the check returned before the probe answered")
	}
	if took > 150*time.Millisecond {
		t.Fatalf("answered after %s, want about checkTimeout", took)
	}
}

// Review Focus 3: a check that ignores its ctx and never returns holds no probe
// past checkTimeout, and no later run waits on it.
func TestCheck_StuckCheckDoesNotBlockProbes(t *testing.T) {
	release := make(chan struct{})
	var entered, left atomic.Int32
	p := health.NewWith(health.Options{CheckTimeout: ms(50), CacheFor: ms(10)})
	p.Check("stuck", func(context.Context) error {
		entered.Add(1)
		defer left.Add(1)
		<-release // ignores its ctx
		return nil
	})
	h := site(t, p, nil)

	const probes = 3
	for i := range probes {
		if i > 0 {
			time.Sleep(30 * time.Millisecond)
		}
		rec, took := probeWithin(t, h, 2*time.Second)
		expect(t, rec, http.StatusServiceUnavailable, "unavailable")
		if took > 200*time.Millisecond {
			t.Fatalf("probe %d answered after %s, want about checkTimeout", i, took)
		}
	}
	// cacheFor is shorter than the gap between probes, so each probe starts its
	// own run, and each run enters the check once.
	if n := entered.Load(); n != probes {
		t.Fatalf("check entered %d times, want %d: one per run", n, probes)
	}

	// Unblock the stuck goroutines so none outlives the test.
	close(release)
	deadline := time.Now().Add(time.Second)
	for left.Load() != entered.Load() {
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d stuck checks returned", left.Load(), entered.Load())
		}
		time.Sleep(time.Millisecond)
	}
}

func TestCheck_ConcurrentProbesShareOneRun(t *testing.T) {
	var calls atomic.Int32
	p := health.New()
	p.Check("db", func(context.Context) error {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond)
		return nil
	})
	h := site(t, p, nil)

	var wg sync.WaitGroup
	codes := make([]int, 20)
	for i := range codes {
		wg.Go(func() { codes[i] = do(h, http.MethodGet, "/readyz").Code })
	}
	wg.Wait()
	for i, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("probe %d: %d, want 200", i, code)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("check called %d times, want 1", n)
	}
}

func TestCheck_CacheFor(t *testing.T) {
	t.Run("fresh", func(t *testing.T) {
		var calls atomic.Int32
		p := health.NewWith(health.Options{CacheFor: health.Duration(time.Hour)})
		p.Check("db", func(context.Context) error { calls.Add(1); return nil })
		h := site(t, p, nil)
		for range 3 {
			expect(t, do(h, http.MethodGet, "/readyz"), http.StatusOK, "ok")
		}
		if n := calls.Load(); n != 1 {
			t.Fatalf("within cacheFor: %d calls, want 1", n)
		}
	})
	t.Run("stale", func(t *testing.T) {
		var calls atomic.Int32
		p := health.NewWith(health.Options{CacheFor: ms(1)})
		p.Check("db", func(context.Context) error { calls.Add(1); return nil })
		h := site(t, p, nil)
		do(h, http.MethodGet, "/readyz")
		time.Sleep(20 * time.Millisecond)
		do(h, http.MethodGet, "/readyz")
		if n := calls.Load(); n != 2 {
			t.Fatalf("after cacheFor: %d calls, want 2", n)
		}
	})
}

// Probes that wait for a run take that run's result, even when it is already
// stale by cacheFor: they do not start a run of their own after it.
func TestCheck_WaitersTakeTheRunsResult(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	p := health.NewWith(health.Options{CacheFor: health.Duration(1)})
	p.Check("db", func(context.Context) error {
		calls.Add(1)
		<-release
		return nil
	})
	h := site(t, p, nil)

	var wg sync.WaitGroup
	codes := make([]int, 20)
	for i := range codes {
		wg.Go(func() { codes[i] = do(h, http.MethodGet, "/readyz").Code })
	}
	// Let every probe reach the run before it ends.
	for calls.Load() == 0 {
		runtime.Gosched()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	for i, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("probe %d: %d, want 200", i, code)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("check called %d times, want 1", n)
	}
}

// A check that answers after its deadline does not change the cached result.
func TestCheck_LateResultDiscarded(t *testing.T) {
	release := make(chan struct{})
	var left atomic.Int32
	p := health.NewWith(health.Options{CheckTimeout: ms(30), CacheFor: health.Duration(time.Hour)})
	p.Check("late", func(context.Context) error { defer left.Add(1); <-release; return nil })
	h := site(t, p, nil)

	rec, _ := probeWithin(t, h, 2*time.Second)
	expect(t, rec, http.StatusServiceUnavailable, "unavailable")
	close(release)
	for left.Load() == 0 {
		runtime.Gosched()
	}
	for range 20 {
		time.Sleep(time.Millisecond)
		expect(t, do(h, http.MethodGet, "/readyz"), http.StatusServiceUnavailable, "unavailable")
	}
}

// A timed-out check's goroutines end once the check returns.
func TestCheck_GoroutinesReturnToBaseline(t *testing.T) {
	release := make(chan struct{})
	var left atomic.Int32
	p := health.NewWith(health.Options{CheckTimeout: ms(20)})
	p.Check("late", func(context.Context) error { defer left.Add(1); <-release; return nil })
	h := site(t, p, nil)
	base := runtime.NumGoroutine()

	rec, _ := probeWithin(t, h, 2*time.Second)
	expect(t, rec, http.StatusServiceUnavailable, "unavailable")
	close(release)
	deadline := time.Now().Add(time.Second)
	for left.Load() == 0 || runtime.NumGoroutine() > base {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines %d, base %d", runtime.NumGoroutine(), base)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Checks run in parallel: two that each wait for the other both pass.
func TestCheck_Parallel(t *testing.T) {
	a, b := make(chan struct{}), make(chan struct{})
	p := health.NewWith(health.Options{CheckTimeout: health.Duration(time.Second)})
	p.Check("a", func(ctx context.Context) error {
		close(a)
		select {
		case <-b:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	p.Check("b", func(ctx context.Context) error {
		close(b)
		select {
		case <-a:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	rec, _ := probeWithin(t, site(t, p, nil), 2*time.Second)
	expect(t, rec, http.StatusOK, "ok")
}

// A probe that started before the drain and answers after it says draining.
func TestCheck_RunAcrossDrain(t *testing.T) {
	release, started := make(chan struct{}), make(chan struct{})
	p := health.NewWith(health.Options{CheckTimeout: health.Duration(5 * time.Second)})
	p.Check("db", func(context.Context) error { close(started); <-release; return nil })
	h := site(t, p, nil)

	first := make(chan *httptest.ResponseRecorder)
	go func() { first <- do(h, http.MethodGet, "/readyz") }()
	<-started
	p.OnDrain()
	close(release)
	expect(t, <-first, http.StatusServiceUnavailable, "draining")
}

// A probe whose request ends does not fail the run: the check's ctx is its own.
func TestCheck_DetachedFromProbeRequest(t *testing.T) {
	p := health.New()
	p.Check("db", func(ctx context.Context) error {
		time.Sleep(20 * time.Millisecond)
		return ctx.Err()
	})
	h := site(t, p, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil).WithContext(ctx))
	expect(t, rec, http.StatusOK, "ok")
	expect(t, do(h, http.MethodGet, "/readyz"), http.StatusOK, "ok")
}

// A probe waiting for another probe's run returns when its own request ends.
func TestCheck_WaitingProbeHonoursItsContext(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	p := health.NewWith(health.Options{CheckTimeout: health.Duration(5 * time.Second)})
	p.Check("slow", func(context.Context) error {
		close(started)
		<-release
		return nil
	})
	h := site(t, p, nil)

	first := make(chan int)
	go func() { first <- do(h, http.MethodGet, "/readyz").Code }()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	rec := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil).WithContext(ctx))
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Fatalf("waiting probe answered after %s, want when its request ended", took)
	}
	expect(t, rec, http.StatusServiceUnavailable, "unavailable")

	close(release)
	if code := <-first; code != http.StatusOK {
		t.Fatalf("the run's own probe: %d, want 200", code)
	}
}

func TestCheck_Details(t *testing.T) {
	p := health.NewWith(health.Options{Details: true})
	p.Check("db", func(context.Context) error { return errors.New("secret-host down") })
	p.Check("cache", func(context.Context) error { return nil })
	h := site(t, p, nil)

	rec := do(h, http.MethodGet, "/readyz")
	expect(t, rec, http.StatusServiceUnavailable, `{"status":"unavailable","checks":{"cache":"ok","db":"failing"}}`)
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type %q, want application/json", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control %q, want no-store", got)
	}

	rec = do(h, http.MethodPost, "/readyz")
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST: %d Allow %q", rec.Code, rec.Header().Get("Allow"))
	}

	p.OnDrain()
	expect(t, do(h, http.MethodGet, "/readyz"), http.StatusServiceUnavailable, `{"status":"draining"}`)
}

func TestCheck_DetailsWithoutChecks(t *testing.T) {
	h := site(t, health.NewWith(health.Options{Details: true}), nil)
	expect(t, do(h, http.MethodGet, "/readyz"), http.StatusOK, `{"status":"ok"}`)
}

func expectPanic(t *testing.T, what string, f func()) {
	t.Helper()
	defer func() {
		t.Helper()
		if recover() == nil {
			t.Errorf("%s did not panic", what)
		}
	}()
	f()
}

func TestCheck_Misuse(t *testing.T) {
	ok := func(context.Context) error { return nil }
	p := health.New()
	expectPanic(t, "an empty name", func() { p.Check("", ok) })
	expectPanic(t, "a nil fn", func() { p.Check("db", nil) })
	p.Check("db", ok)
	expectPanic(t, "a duplicate", func() { p.Check("db", ok) })
	site(t, p, nil)
	expectPanic(t, "a call after Init", func() { p.Check("cache", ok) })
}

// A check still running at checkTimeout has its context cancelled, so one that
// honours it can stop.
func TestCheck_TimeoutCancelsTheContext(t *testing.T) {
	got := make(chan error, 1)
	p := health.NewWith(health.Options{CheckTimeout: ms(50)})
	p.Check("waits", func(ctx context.Context) error {
		<-ctx.Done()
		got <- ctx.Err()
		return ctx.Err()
	})
	h := site(t, p, nil)
	start := time.Now()
	go do(h, http.MethodGet, "/readyz")
	select {
	case err := <-got:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the check's context ended with %v, want context.DeadlineExceeded", err)
		}
		if took := time.Since(start); took > 500*time.Millisecond {
			t.Fatalf("the context ended after %s, want about checkTimeout", took)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the check's context was never cancelled")
	}
}

// records is a slog.Handler keeping every record it is handed.
type records struct {
	mu   sync.Mutex
	list []slog.Record
}

func (*records) Enabled(context.Context, slog.Level) bool { return true }
func (r *records) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.list = append(r.list, rec.Clone())
	return nil
}
func (r *records) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *records) WithGroup(string) slog.Handler      { return r }

// count is how many records at level carry message and the attribute check=name.
func (r *records) count(level slog.Level, message, name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, rec := range r.list {
		if rec.Level != level || rec.Message != message {
			continue
		}
		rec.Attrs(func(a slog.Attr) bool {
			if a.Key == "check" && a.Value.String() == name {
				n++
				return false
			}
			return true
		})
	}
	return n
}

// A failing check is logged at Warn and a panicking one at Error, each once and
// with its name, through the application's logger.
func TestCheck_Logging(t *testing.T) {
	logs := &records{}
	p := health.New()
	p.Check("db", func(context.Context) error { return errors.New("refused") })
	p.Check("boom", func(context.Context) error { panic("kaboom") })
	a, err := collage.New(&collage.Config{
		Server: collage.ServerConfig{Host: "localhost", Port: 3000},
		Logger: slog.New(logs),
		Template: collage.TemplateConfig{FS: fstest.MapFS{
			"t/p.html": {Data: []byte(`<main>home</main>`)},
		}, Root: "t"},
		Plugins: []collage.Plugin{p},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	expect(t, do(a.Handler(), http.MethodGet, "/readyz"), http.StatusServiceUnavailable, "unavailable")

	if n := logs.count(slog.LevelWarn, "elagoht/health: check failed", "db"); n != 1 {
		t.Errorf("%d Warn records for the failing check, want 1", n)
	}
	if n := logs.count(slog.LevelError, "elagoht/health: check panicked", "boom"); n != 1 {
		t.Errorf("%d Error records for the panicking check, want 1", n)
	}
	if n := logs.count(slog.LevelWarn, "elagoht/health: check failed", "boom"); n != 0 {
		t.Errorf("the panic was logged again at Warn, %d times", n)
	}
}
