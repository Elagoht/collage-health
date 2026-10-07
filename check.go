package health

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// check is one application check registered with Check.
type check struct {
	name string
	fn   func(context.Context) error
}

// checks holds the registered checks and the last readiness run. One run is in
// flight at most: a probe that finds the result stale and a run already going
// waits for that run and takes its result instead of starting its own.
type checks struct {
	mu       sync.Mutex
	list     []check
	names    map[string]bool
	last     time.Time       // when result was stored
	result   map[string]bool // name → passed, from the last run; never mutated once stored
	inflight *flight         // the run in flight, or nil when none is
}

// flight is one run. result is written once, before done is closed, and only
// read after it.
type flight struct {
	done   chan struct{}
	result map[string]bool
}

// Check registers fn as a readiness check under name. Readiness is 503
// "unavailable" while any check fails. Checks run in parallel, each under a
// context that ends after checkTimeout, detached from the probe's request.
// A check fails when it returns an error, panics, or has not returned by then;
// the error is logged with the check's name, never written to the response.
//
// A check should honour its context. One that does not keeps its goroutine
// until it returns, but readiness does not wait for it past checkTimeout.
//
// Check panics on an empty name, a name already registered, a nil fn, or a call
// after Init: the checks are fixed once the application starts.
func (p *Plugin) Check(name string, fn func(context.Context) error) {
	switch {
	case name == "":
		panic("elagoht/health: Check needs a name")
	case fn == nil:
		panic(fmt.Sprintf("elagoht/health: Check %q has a nil function", name))
	case p.inited.Load():
		panic(fmt.Sprintf("elagoht/health: Check %q after Init; register checks before the application starts", name))
	}
	c := &p.checks
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.names[name] {
		panic(fmt.Sprintf("elagoht/health: Check %q registered twice", name))
	}
	if c.names == nil {
		c.names = map[string]bool{}
	}
	c.names[name] = true
	c.list = append(c.list, check{name: name, fn: fn})
}

// results returns each check's result, from the cache when it is younger than
// cacheFor, else from the run in flight, else from a new run. A probe that waited
// for a run takes that run's result as it is, without checking its age again, so
// waiting probes never chain runs. ok is false when ctx ended while the probe
// waited for another probe's run.
func (p *Plugin) results(ctx context.Context) (result map[string]bool, ok bool) {
	c := &p.checks
	c.mu.Lock()
	if c.result != nil && time.Since(c.last) < time.Duration(p.opts.CacheFor) {
		result := c.result
		c.mu.Unlock()
		return result, true
	}
	if f := c.inflight; f != nil {
		c.mu.Unlock()
		select {
		case <-f.done:
			return f.result, true
		case <-ctx.Done():
			return nil, false
		}
	}
	f := &flight{done: make(chan struct{})}
	c.inflight = f
	list := c.list
	c.mu.Unlock()

	f.result = p.run(list)

	c.mu.Lock()
	c.result, c.last = f.result, time.Now()
	c.inflight = nil
	c.mu.Unlock()
	close(f.done)
	return f.result, true
}

// run executes every check in parallel and waits for each until its own
// deadline. A check still running then counts as failing, and its goroutine is
// left to finish on its own: its buffered channel takes the late result, which
// nobody reads, so the goroutine never blocks on it. A check that ignores its
// context and never returns leaks that one goroutine, and each later run that
// reaches it starts another; readiness itself never waits past checkTimeout.
func (p *Plugin) run(list []check) map[string]bool {
	timeout := time.Duration(p.opts.CheckTimeout)
	result := make(map[string]bool, len(list))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, ch := range list {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			out := make(chan error, 1)
			go func() { out <- p.call(ctx, ch) }()
			var err error
			select {
			case err = <-out:
			case <-ctx.Done():
				err = fmt.Errorf("no answer within %s", timeout)
			}
			if err != nil && !errors.Is(err, errPanicked) {
				p.logger.Warn("elagoht/health: check failed", "check", ch.name, "error", err)
			}
			mu.Lock()
			result[ch.name] = err == nil
			mu.Unlock()
		}()
	}
	wg.Wait()
	return result
}

// errPanicked marks a check that panicked; the panic itself is logged by call.
var errPanicked = errors.New("panicked")

// call runs one check, turning a panic into a failure.
func (p *Plugin) call(ctx context.Context, ch check) (err error) {
	defer func() {
		if v := recover(); v != nil {
			p.logger.Error("elagoht/health: check panicked", "check", ch.name, "panic", fmt.Sprint(v))
			err = errPanicked
		}
	}()
	return ch.fn(ctx)
}
