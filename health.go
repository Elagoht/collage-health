// Package health answers liveness and readiness probes for a collage
// application, and turns readiness false while the server drains.
package health

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Elagoht/collage/pkg/collage"
)

// Name is the plugin's name.
const Name = "elagoht/health"

const version = "0.1.0"

// Duration is a time.Duration that reads from configuration as Go writes one,
// "2s", as well as a number of nanoseconds.
type Duration time.Duration

// UnmarshalJSON reads "2s" or a number of nanoseconds.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		v, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("elagoht/health: %w", err)
		}
		*d = Duration(v)
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return errors.New(`elagoht/health: a duration is a string like "2s" or a number of nanoseconds`)
	}
	*d = Duration(n)
	return nil
}

// Options configures the plugin.
type Options struct {
	LivePath     string   `json:"livePath"`     // liveness endpoint, default "/healthz"
	ReadyPath    string   `json:"readyPath"`    // readiness endpoint, default "/readyz"
	CheckTimeout Duration `json:"checkTimeout"` // per check, default "2s"
	CacheFor     Duration `json:"cacheFor"`     // how long one readiness result is reused, default "1s"
	Details      bool     `json:"details"`      // a JSON body naming each check, default false
	MaxInFlight  int      `json:"maxInFlight"`  // requests served at once at most, default 0: no cap
}

// Plugin is the health plugin.
type Plugin struct {
	opts Options

	inited   atomic.Bool // set once Init has validated the options
	draining atomic.Bool // set by OnDrain
	logger   *slog.Logger
	checks   checks
}

var (
	_ collage.Plugin    = (*Plugin)(nil)
	_ collage.DrainHook = (*Plugin)(nil)
)

// New returns the plugin with its defaults, which configuration may change.
func New() *Plugin { return &Plugin{} }

// NewWith returns the plugin with opts as the defaults configuration starts from.
func NewWith(opts Options) *Plugin { return &Plugin{opts: opts} }

// Name returns Name.
func (p *Plugin) Name() string { return Name }

// Version returns the plugin's version.
func (p *Plugin) Version() string { return version }

// Init reads the configuration, applies the defaults, validates them, refuses an
// endpoint path a page also claims, and adds the middleware that answers the
// endpoints.
func (p *Plugin) Init(_ context.Context, host collage.Host) error {
	opts, err := collage.PluginConfig(host, p.opts)
	if err != nil {
		return err
	}
	if opts.LivePath == "" {
		opts.LivePath = "/healthz"
	}
	if opts.ReadyPath == "" {
		opts.ReadyPath = "/readyz"
	}
	if opts.CheckTimeout == 0 {
		opts.CheckTimeout = Duration(2 * time.Second)
	}
	if opts.CacheFor == 0 {
		opts.CacheFor = Duration(time.Second)
	}
	if err := opts.validate(); err != nil {
		return err
	}
	for _, page := range host.Pages() {
		// Paths hold no locale prefix and Host cannot say which applies, so a
		// page in any locale is refused.
		for locale, pattern := range page.Paths {
			for _, path := range []string{opts.LivePath, opts.ReadyPath} {
				if pattern == path || strings.TrimSuffix(pattern, "/") == strings.TrimSuffix(path, "/") {
					return fmt.Errorf("elagoht/health: %s is also page %q's path (locale %q); the endpoint would shadow it — move the endpoint with livePath/readyPath", path, page.Name, locale)
				}
			}
		}
	}
	p.opts = opts
	p.logger = host.Logger()
	if p.logger == nil {
		p.logger = slog.Default()
	}
	p.inited.Store(true)
	return host.Use(p.middleware)
}

// validate checks the options once the defaults are in.
func (o *Options) validate() error {
	switch {
	case !strings.HasPrefix(o.LivePath, "/"):
		return fmt.Errorf(`elagoht/health: livePath %q must start with "/"`, o.LivePath)
	case !strings.HasPrefix(o.ReadyPath, "/"):
		return fmt.Errorf(`elagoht/health: readyPath %q must start with "/"`, o.ReadyPath)
	case o.LivePath == o.ReadyPath:
		return fmt.Errorf("elagoht/health: livePath and readyPath are both %s", o.LivePath)
	case o.CheckTimeout < 0:
		return errors.New("elagoht/health: checkTimeout must not be negative")
	case o.CacheFor < 0:
		return errors.New("elagoht/health: cacheFor must not be negative")
	case o.MaxInFlight < 0:
		return fmt.Errorf("elagoht/health: maxInFlight %d must not be negative", o.MaxInFlight)
	}
	return nil
}

// Shutdown releases nothing: the plugin holds no resource.
func (p *Plugin) Shutdown(context.Context) error { return nil }

// OnDrain turns readiness false: from now on readyPath answers 503 "draining".
func (p *Plugin) OnDrain() { p.draining.Store(true) }

// middleware answers the endpoints before routing and passes every other
// request on.
func (p *Plugin) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case p.opts.LivePath:
			answer(w, r, http.StatusOK, "ok")
		case p.opts.ReadyPath:
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				answer(w, r, http.StatusMethodNotAllowed, "")
				return
			}
			status, result := p.ready(r.Context())
			code := http.StatusOK
			if status != "ok" {
				code = http.StatusServiceUnavailable
			}
			if p.opts.Details {
				answerDetails(w, r, code, status, result)
				return
			}
			answer(w, r, code, status)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// ready is the readiness status, "ok", "unavailable" or "draining", and each
// check's result. While draining the checks are not run. A probe whose request
// ends while it waits for a run another probe started gets "unavailable".
func (p *Plugin) ready(ctx context.Context) (string, map[string]bool) {
	if p.draining.Load() {
		return "draining", nil
	}
	result, ok := p.results(ctx)
	if !ok {
		return "unavailable", nil
	}
	for _, passed := range result {
		if !passed {
			return "unavailable", result
		}
	}
	return "ok", result
}

// report is the details body. encoding/json writes a map's keys sorted, so the
// checks come out by name.
type report struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

// answerDetails writes the readiness response as JSON: the status and each
// check as "ok" or "failing", never an error's text.
func answerDetails(w http.ResponseWriter, r *http.Request, code int, status string, result map[string]bool) {
	rep := report{Status: status}
	if len(result) > 0 {
		rep.Checks = make(map[string]string, len(result))
		for name, passed := range result {
			rep.Checks[name] = "failing"
			if passed {
				rep.Checks[name] = "ok"
			}
		}
	}
	body, err := json.Marshal(rep)
	if err != nil { // a struct of strings always marshals
		body = []byte(`{"status":"unavailable"}`)
		code = http.StatusServiceUnavailable
	}
	write(w, r, code, "application/json", string(body))
}

// answer writes an endpoint's plain-text response: GET and HEAD only, never
// cached, and no body for HEAD.
func answer(w http.ResponseWriter, r *http.Request, code int, body string) {
	write(w, r, code, "text/plain; charset=utf-8", body)
}

// write writes an endpoint's response with contentType. Any method but GET and
// HEAD gets 405 instead, as plain text.
func write(w http.ResponseWriter, r *http.Request, code int, contentType, body string) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	switch r.Method {
	case http.MethodGet, http.MethodHead:
	default:
		h.Set("Allow", "GET, HEAD")
		code, body, contentType = http.StatusMethodNotAllowed, "method not allowed", "text/plain; charset=utf-8"
	}
	h.Set("Content-Type", contentType)
	w.WriteHeader(code)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(body))
	}
}
