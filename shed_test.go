package health_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	health "github.com/Elagoht/collage-health"
	"github.com/Elagoht/collage/pkg/collage"
)

// holder is a plugin listed after health, so its middleware runs inside health's.
// A request to "/hold-<group>-<n>" reports on entered and waits until the group
// is released; "/panic" panics, which unwinds through health's middleware to the
// core's recovery. Every other request goes on to the router.
type holder struct {
	entered chan string
	mu      sync.Mutex
	gates   map[string]chan struct{}
}

func newHolder() *holder {
	return &holder{entered: make(chan string, 64), gates: map[string]chan struct{}{}}
}

// gate is group's release channel, made on first use.
func (h *holder) gate(group string) chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	g, ok := h.gates[group]
	if !ok {
		g = make(chan struct{})
		h.gates[group] = g
	}
	return g
}

// open releases group's held requests.
func (h *holder) open(group string) { close(h.gate(group)) }

func (*holder) Name() string                   { return "test/holder" }
func (*holder) Version() string                { return "0" }
func (*holder) Shutdown(context.Context) error { return nil }
func (h *holder) Init(_ context.Context, host collage.Host) error {
	return host.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasPrefix(r.URL.Path, "/hold-"):
				group, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/hold-"), "-")
				gate := h.gate(group)
				h.entered <- r.URL.Path
				<-gate
				w.WriteHeader(http.StatusOK)
			case r.URL.Path == "/panic":
				panic("test: a handler panicked")
			default:
				next.ServeHTTP(w, r)
			}
		})
	})
}

// shedApp builds and starts an application with health p, then the holder, a home
// page at "/" and a page at "/boom" whose data handler panics.
func shedApp(t *testing.T, p *health.Plugin, hold *holder) *collage.App {
	t.Helper()
	a, err := collage.New(&collage.Config{
		Server: collage.ServerConfig{Host: "localhost", Port: 3000},
		Logger: slog.New(slog.DiscardHandler),
		Template: collage.TemplateConfig{FS: fstest.MapFS{
			"t/p.html": {Data: []byte(`<main>home</main>`)},
		}, Root: "t"},
		Plugins: []collage.Plugin{p, hold},
	})
	if err != nil {
		t.Fatal(err)
	}
	home := collage.NewPage("home").WithContent(collage.NewFragment("home", "p.html").Build()).WithPath("en", "/").Build()
	boom := collage.NewPage("boom").WithContent(collage.NewFragment("boom", "p.html").WithData(
		collage.Load(func(context.Context, *collage.RenderContext) (string, error) { panic("test: a data handler panicked") }),
	).Build()).WithPath("en", "/boom").Build()
	for _, page := range []*collage.Page{home, boom} {
		if err := a.RegisterPage(page); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

// holdN starts n requests in group, each to its own path, and waits until all
// are inside. The returned function lets them go and waits for their answers,
// each of which must be 200.
func holdN(t *testing.T, h http.Handler, hold *holder, prefix string, n int) (release func()) {
	t.Helper()
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := do(h, http.MethodGet, "/hold-"+prefix+"-"+string(rune('a'+i)))
			if rec.Code != http.StatusOK {
				t.Errorf("held request: %d, want 200", rec.Code)
			}
		}()
	}
	for range n {
		select {
		case <-hold.entered:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: the held requests did not all get in", prefix)
		}
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			hold.open(prefix)
			wg.Wait()
		})
	}
	t.Cleanup(release) // a failed test still lets its held requests go
	return release
}

func expectShed(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	expect(t, rec, http.StatusServiceUnavailable, "overloaded")
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After %q, want 1", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control %q, want no-store", got)
	}
}

// Review Focus 4: past the cap a request is refused, the probes never are, and
// neither a refusal nor a panic leaks a slot.
func TestShed_EndpointsExemptAndCounterBalanced(t *testing.T) {
	hold := newHolder()
	a := shedApp(t, health.NewWith(health.Options{MaxInFlight: 2}), hold)
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	h := a.Handler()

	// Two in, the cap reached.
	release := holdN(t, h, hold, "first", 2)

	expectShed(t, do(h, http.MethodGet, "/"))
	expectShed(t, do(h, http.MethodPost, "/")) // shed, not 405: the cap is not an endpoint
	expect(t, do(h, http.MethodGet, "/readyz"), http.StatusOK, "ok")
	expect(t, do(h, http.MethodGet, "/healthz"), http.StatusOK, "ok")
	expect(t, do(h, http.MethodHead, "/readyz"), http.StatusOK, "")

	release()
	// The held requests have answered, and their slots are back.
	expect(t, do(h, http.MethodGet, "/"), http.StatusOK, "<main>home</main>")

	// The refusals above took no slot: two can be held again at once.
	holdN(t, h, hold, "second", 2)()

	// A panic unwinding through the middleware gives its slot back, and so does a
	// data handler's panic, which the render turns into a 500.
	for _, path := range []string{"/panic", "/boom", "/panic", "/boom"} {
		if rec := do(h, http.MethodGet, path); rec.Code != http.StatusInternalServerError {
			t.Fatalf("%s: %d, want 500", path, rec.Code)
		}
	}
	release = holdN(t, h, hold, "third", 2)
	expectShed(t, do(h, http.MethodGet, "/"))
	release()
	expect(t, do(h, http.MethodGet, "/"), http.StatusOK, "<main>home</main>")
}

// With no cap nothing is counted or refused.
func TestShed_Off(t *testing.T) {
	hold := newHolder()
	a := shedApp(t, health.New(), hold)
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	h := a.Handler()
	release := holdN(t, h, hold, "off", 10)

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rec := do(h, http.MethodGet, "/"); rec.Code != http.StatusOK {
				t.Errorf("with no cap: %d %q, want 200", rec.Code, rec.Body.String())
			}
		}()
	}
	wg.Wait()
	release()
}

// Draining answers without running the checks.
func TestCheck_NotRunWhileDraining(t *testing.T) {
	p := health.New()
	var mu sync.Mutex
	calls := 0
	p.Check("counted", func(context.Context) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return nil
	})
	h := site(t, p, nil)
	p.OnDrain()
	for range 3 {
		expect(t, do(h, http.MethodGet, "/readyz"), http.StatusServiceUnavailable, "draining")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Fatalf("the check ran %d times while draining, want 0", calls)
	}
}
