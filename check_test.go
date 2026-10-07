package health_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	health "github.com/Elagoht/collage-health"
)

func ms(n int) health.Duration { return health.Duration(time.Duration(n) * time.Millisecond) }

// probe is a readiness probe and how long it took.
func probe(h http.Handler) (*httptest.ResponseRecorder, time.Duration) {
	start := time.Now()
	rec := do(h, http.MethodGet, "/readyz")
	return rec, time.Since(start)
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
	p := health.NewWith(health.Options{CheckTimeout: ms(50)})
	p.Check("slow", func(context.Context) error { time.Sleep(200 * time.Millisecond); return nil })
	h := site(t, p, nil)

	rec, took := probe(h)
	expect(t, rec, http.StatusServiceUnavailable, "unavailable")
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
		rec, took := probe(h)
		expect(t, rec, http.StatusServiceUnavailable, "unavailable")
		if took > 200*time.Millisecond {
			t.Fatalf("probe %d answered after %s, want about checkTimeout", i, took)
		}
	}
	if n := entered.Load(); n < 1 || n > probes {
		t.Fatalf("check entered %d times, want 1..%d: one per run at most", n, probes)
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
	var calls atomic.Int32
	p := health.NewWith(health.Options{CacheFor: ms(100)})
	p.Check("db", func(context.Context) error { calls.Add(1); return nil })
	h := site(t, p, nil)

	do(h, http.MethodGet, "/readyz")
	time.Sleep(10 * time.Millisecond)
	do(h, http.MethodGet, "/readyz")
	if n := calls.Load(); n != 1 {
		t.Fatalf("within cacheFor: %d calls, want 1", n)
	}
	time.Sleep(150 * time.Millisecond)
	do(h, http.MethodGet, "/readyz")
	if n := calls.Load(); n != 2 {
		t.Fatalf("after cacheFor: %d calls, want 2", n)
	}
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
