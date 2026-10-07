// Package health answers liveness and readiness probes for a collage
// application, and turns readiness false while the server drains.
package health

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		for _, pattern := range page.Paths {
			for _, path := range []string{opts.LivePath, opts.ReadyPath} {
				if pattern == path || strings.TrimSuffix(pattern, "/") == strings.TrimSuffix(path, "/") {
					return fmt.Errorf("elagoht/health: %s is also page %q's path; the middleware answers first, so the page could never be reached", path, page.Name)
				}
			}
		}
	}
	p.opts = opts
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
			status, _ := p.ready(r.Context())
			code := http.StatusOK
			if status != "ok" {
				code = http.StatusServiceUnavailable
			}
			answer(w, r, code, status)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// ready is the readiness status, "ok" or "draining", and each check's result.
func (p *Plugin) ready(context.Context) (string, map[string]bool) {
	if p.draining.Load() {
		return "draining", nil
	}
	return "ok", nil
}

// answer writes an endpoint's plain-text response: GET and HEAD only, never
// cached, and no body for HEAD.
func answer(w http.ResponseWriter, r *http.Request, code int, body string) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	switch r.Method {
	case http.MethodGet, http.MethodHead:
	default:
		h.Set("Allow", "GET, HEAD")
		code, body = http.StatusMethodNotAllowed, "method not allowed"
	}
	w.WriteHeader(code)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(body))
	}
}
