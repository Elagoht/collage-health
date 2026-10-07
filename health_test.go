package health_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"

	health "github.com/Elagoht/collage-health"
	"github.com/Elagoht/collage/pkg/collage"
)

// app builds an application with the plugin and a home page at "/", plus a page
// at each of extra, without starting it.
func app(t *testing.T, p *health.Plugin, config map[string]json.RawMessage, extra ...string) *collage.App {
	t.Helper()
	a, err := collage.New(&collage.Config{
		Server: collage.ServerConfig{Host: "localhost", Port: 3000},
		Logger: slog.New(slog.DiscardHandler),
		Template: collage.TemplateConfig{FS: fstest.MapFS{
			"t/p.html": {Data: []byte(`<main>home</main>`)},
		}, Root: "t"},
		Plugins:      []collage.Plugin{p},
		PluginConfig: config,
	})
	if err != nil {
		t.Fatal(err)
	}
	pages := map[string]string{"home": "/"}
	for _, path := range extra {
		pages["page"+strings.ReplaceAll(path, "/", "-")] = path
	}
	for name, path := range pages {
		if err := a.RegisterPage(collage.NewPage(name).WithContent(collage.NewFragment(name, "p.html").Build()).WithPath("en", path).Build()); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

// site is app's handler, after a start that must succeed.
func site(t *testing.T, p *health.Plugin, config map[string]json.RawMessage) http.Handler {
	t.Helper()
	a := app(t, p, config)
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return a.Handler()
}

func do(h http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func expect(t *testing.T, rec *httptest.ResponseRecorder, status int, body string) {
	t.Helper()
	if rec.Code != status || rec.Body.String() != body {
		t.Fatalf("got %d %q, want %d %q", rec.Code, rec.Body.String(), status, body)
	}
}

func expectEndpointHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control %q, want no-store", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type %q, want text/plain; charset=utf-8", got)
	}
}

func TestLiveness(t *testing.T) {
	p := health.New()
	var calls atomic.Int32
	p.Check("counted", func(context.Context) error { calls.Add(1); return nil })
	h := site(t, p, nil)

	rec := do(h, http.MethodGet, "/healthz")
	expect(t, rec, http.StatusOK, "ok")
	expectEndpointHeaders(t, rec)

	rec = do(h, http.MethodHead, "/healthz")
	expect(t, rec, http.StatusOK, "")
	expectEndpointHeaders(t, rec)

	rec = do(h, http.MethodPost, "/healthz")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow %q, want GET, HEAD", got)
	}
	expectEndpointHeaders(t, rec)

	// Liveness runs no checks, whatever the method.
	for range 3 {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
			do(h, method, "/healthz")
		}
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("liveness ran the check %d times, want 0", n)
	}
}

// A method readiness refuses runs no checks.
func TestReadiness_RefusedMethodRunsNoChecks(t *testing.T) {
	p := health.New()
	var calls atomic.Int32
	p.Check("counted", func(context.Context) error { calls.Add(1); return nil })
	h := site(t, p, nil)
	for range 3 {
		rec := do(h, http.MethodPost, "/readyz")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST /readyz: %d, want 405", rec.Code)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("POST /readyz ran the check %d times, want 0", n)
	}
}

func TestReadinessAndDrain(t *testing.T) {
	p := health.New()
	h := site(t, p, nil)

	rec := do(h, http.MethodGet, "/readyz")
	expect(t, rec, http.StatusOK, "ok")
	expectEndpointHeaders(t, rec)

	p.OnDrain()
	rec = do(h, http.MethodGet, "/readyz")
	expect(t, rec, http.StatusServiceUnavailable, "draining")
	expectEndpointHeaders(t, rec)

	if rec := do(h, http.MethodGet, "/"); rec.Code != http.StatusOK {
		t.Fatalf("page while draining: %d, want 200", rec.Code)
	}
	// Liveness is not readiness: a draining process is still alive.
	expect(t, do(h, http.MethodGet, "/healthz"), http.StatusOK, "ok")
}

func TestCustomPaths(t *testing.T) {
	h := site(t, health.NewWith(health.Options{LivePath: "/live", ReadyPath: "/ready"}), nil)
	expect(t, do(h, http.MethodGet, "/live"), http.StatusOK, "ok")
	expect(t, do(h, http.MethodGet, "/ready"), http.StatusOK, "ok")
	if rec := do(h, http.MethodGet, "/healthz"); rec.Code != http.StatusNotFound {
		t.Fatalf("/healthz: %d, want 404 from the app", rec.Code)
	}
}

func TestInit_Validation(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts health.Options
		want string
	}{
		{"equal paths", health.Options{LivePath: "/h", ReadyPath: "/h"}, "livePath and readyPath are both"},
		{"livePath without a slash", health.Options{LivePath: "healthz"}, `livePath "healthz" must start with "/"`},
		{"readyPath without a slash", health.Options{ReadyPath: "readyz"}, `readyPath "readyz" must start with "/"`},
		{"negative checkTimeout", health.Options{CheckTimeout: -1}, "checkTimeout must not be negative"},
		{"negative cacheFor", health.Options{CacheFor: -1}, "cacheFor must not be negative"},
		{"negative maxInFlight", health.Options{MaxInFlight: -1}, "maxInFlight -1 must not be negative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := app(t, health.NewWith(tc.opts), nil).Start()
			if err == nil {
				t.Fatal("Start succeeded")
			}
			if !strings.Contains(err.Error(), "elagoht/health: "+tc.want) {
				t.Fatalf("error %q, want it to contain %q", err, "elagoht/health: "+tc.want)
			}
		})
	}
}

func TestInit_PathClaimedByPage(t *testing.T) {
	for _, path := range []string{"/healthz", "/readyz/"} {
		t.Run(path, func(t *testing.T) {
			err := app(t, health.New(), nil, path).Start()
			if err == nil {
				t.Fatal("Start succeeded")
			}
			page := "page" + strings.ReplaceAll(path, "/", "-")
			if !strings.Contains(err.Error(), "elagoht/health: ") || !strings.Contains(err.Error(), `page "`+page+`"'s path (locale "en")`) {
				t.Fatalf("error %q, want it to name page %q", err, page)
			}
		})
	}
}

// refuseAll is a plugin whose middleware answers 401 to every request, as
// basicauth does to a request without credentials.
type refuseAll struct{}

func (refuseAll) Name() string                   { return "test/refuseall" }
func (refuseAll) Version() string                { return "0" }
func (refuseAll) Shutdown(context.Context) error { return nil }
func (refuseAll) Init(_ context.Context, host collage.Host) error {
	return host.Use(func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		})
	})
}

// Listed first, the plugin answers probes before a plugin listed after it can
// refuse them.
func TestListedFirstAnswersBeforeLaterPlugins(t *testing.T) {
	p := health.New()
	a, err := collage.New(&collage.Config{
		Server: collage.ServerConfig{Host: "localhost", Port: 3000},
		Logger: slog.New(slog.DiscardHandler),
		Template: collage.TemplateConfig{FS: fstest.MapFS{
			"t/p.html": {Data: []byte(`<main>home</main>`)},
		}, Root: "t"},
		Plugins: []collage.Plugin{p, refuseAll{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.RegisterPage(collage.NewPage("home").WithContent(collage.NewFragment("home", "p.html").Build()).WithPath("en", "/").Build()); err != nil {
		t.Fatal(err)
	}
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	h := a.Handler()
	expect(t, do(h, http.MethodGet, "/healthz"), http.StatusOK, "ok")
	expect(t, do(h, http.MethodGet, "/readyz"), http.StatusOK, "ok")
	if rec := do(h, http.MethodGet, "/"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("/: %d, want 401 from the later plugin", rec.Code)
	}
}

func TestConfigFromJSON(t *testing.T) {
	h := site(t, health.New(), map[string]json.RawMessage{
		health.Name: json.RawMessage(`{"readyPath":"/r","checkTimeout":"500ms"}`),
	})
	expect(t, do(h, http.MethodGet, "/r"), http.StatusOK, "ok")
	expect(t, do(h, http.MethodGet, "/healthz"), http.StatusOK, "ok")

	// A section naming no registered plugin fails as collage reports it.
	err := app(t, health.New(), map[string]json.RawMessage{
		"elagoht/healthz": json.RawMessage(`{"readyPath":"/r"}`),
	}).Start()
	if !errors.Is(err, collage.ErrUnknownPluginConfig) {
		t.Fatalf("unknown key: %v, want ErrUnknownPluginConfig", err)
	}

	// A duration that is neither a Go duration nor nanoseconds fails.
	err = app(t, health.New(), map[string]json.RawMessage{
		health.Name: json.RawMessage(`{"checkTimeout":"soon"}`),
	}).Start()
	if err == nil || !strings.Contains(err.Error(), "elagoht/health: ") {
		t.Fatalf("bad duration: %v", err)
	}
}

func TestDurationNanoseconds(t *testing.T) {
	var d health.Duration
	if err := json.Unmarshal([]byte(`1500000000`), &d); err != nil || d != health.Duration(1500000000) {
		t.Fatalf("got %v, %v", d, err)
	}
	if err := json.Unmarshal([]byte(`true`), &d); err == nil {
		t.Fatal("a boolean was accepted")
	}
}

// Compile-time: the plugin is told when a drain starts.
var _ collage.DrainHook = health.New()

// One Plugin value serves one App: a second App starting with it fails, rather
// than inheriting the first App's drain and answering draining for good.
func TestInit_SecondAppRefused(t *testing.T) {
	p := health.New()
	first := app(t, p, nil)
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	if err := first.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := app(t, p, nil).Start()
	if err == nil || !strings.Contains(err.Error(), "a Plugin value serves one App; create another with New") {
		t.Fatalf("second App's Start: %v, want the one-App error", err)
	}
}

// An Init that failed does not count: the value may be started again.
func TestInit_RetryAfterFailedInit(t *testing.T) {
	p := health.New()
	bad := map[string]json.RawMessage{health.Name: json.RawMessage(`{"livePath":"nope"}`)}
	if err := app(t, p, bad).Start(); err == nil {
		t.Fatal("a livePath without a leading slash started")
	}
	h := site(t, p, nil)
	expect(t, do(h, http.MethodGet, "/readyz"), http.StatusOK, "ok")
}
