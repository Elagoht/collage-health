# elagoht/health

A collage plugin that answers a platform's liveness and readiness probes. Liveness
says the process serves requests. Readiness says it should get traffic, and turns
false the moment collage starts draining for a shutdown, so a load balancer stops
sending requests before the port closes. The endpoints are answered by the plugin's
middleware, not by pages or documents, so a static export never contains them.

```sh
go get github.com/Elagoht/collage-health
```

```go
app, err := collage.New(&collage.Config{
	Server: collage.ServerConfig{DrainDelay: 5 * time.Second, ShutdownTimeout: 10 * time.Second},
	Plugins: []collage.Plugin{
		health.New(), // first: nothing stands between a probe and its answer
		basicauth.New(basicauth.Options{}),
		ratelimit.New(ratelimit.Options{}),
	},
})
```

Requires collage v0.53.0 or later, which tells plugins a drain has started
(`collage.DrainHook`) and has `ServerConfig.DrainDelay`.

## List it first

Middleware runs in the order it was registered, the first outermost, and a plugin
in `Config.Plugins` counts as registered where it is listed; all of them are
outside the middleware the application adds with `app.Use`. List `elagoht/health`
first in `Config.Plugins`, so a probe is answered before basicauth asks it for a
password, ratelimit counts it, or fail2ban bans the probing address. A plugin added
later with `app.RegisterPlugin` sits inside the middleware added before it, so it
is not outermost there.

Plugins implementing `collage.RequestHook`, such as accesslog and otel, run before
any middleware, so they still see every probe; filter the probe paths there if
they are noise.

## Endpoints

| Path | Answers |
| --- | --- |
| `/healthz` (`livePath`) | `200 ok` whenever the process serves a request. It runs no checks: restarting a process whose database is down does not bring the database back |
| `/readyz` (`readyPath`) | `200 ok`; `503 unavailable` while a [check](#checks) fails; `503 draining` once a drain has started |

Both answer `GET` and `HEAD` only, as `text/plain; charset=utf-8` (readiness as
`application/json` with `details`) and with `Cache-Control: no-store`. Any other method gets `405` with `Allow: GET, HEAD`.

The middleware answers before routing, so the paths belong to the plugin:

- A page the application, or a plugin listed before health, registers at either
  path fails startup, with an error naming the page. The check compares the
  page's path patterns as written and ignores locale prefixes, so a page at
  `/healthz` in any locale is refused.
- A page whose pattern matches an endpoint path, such as `/{slug}`, is not
  refused; it simply loses that one value.
- A document or a handler at either path is not checked, and is shadowed: the
  middleware answers and it is never reached. A project scaffolded with the demos
  has a `/healthz` document; delete it, or move the liveness endpoint with
  `livePath`.

## Checks

Register the application's own readiness checks before the application starts:

```go
h := health.New()
h.Check("db", func(ctx context.Context) error { return db.PingContext(ctx) })
h.Check("cache", func(ctx context.Context) error { return rdb.Ping(ctx).Err() })
```

Readiness is `503 unavailable` while any check fails. Liveness runs no checks.

- **Parallel and timed.** A probe runs every check at once, each under a context
  that ends after `checkTimeout`. A check fails when it returns an error, panics,
  or has not returned by then. The probe answers within about `checkTimeout`,
  however many checks there are.
- **Detached from the probe.** The context is not the probe's request: a probe
  that disconnects does not cancel the checks other probes are waiting for.
- **Cached and shared.** A result is reused for `cacheFor`. Probes that arrive
  while a run is in flight wait for that run and take its result instead of
  starting their own, so twenty probes at once run each check once, however
  short `cacheFor` is. A waiting probe whose own request ends gets
  `503 unavailable`.
- **Never the error's text.** An error is logged with the check's name, and a
  panic as well, through the application's logger. The response says only
  `unavailable`, or with `details` which check is `failing`.
- **Not while draining.** Once a drain has started, readiness is `503 draining`
  and the checks are not run. A probe whose checks were running when the drain
  started answers `503 draining` too.

A check should honour its context. One that ignores it and never returns does
not hold readiness: the run counts it as failing at `checkTimeout` and moves on.
Its goroutine is left running until the check returns, though, and each later run
starts the check again, so a check stuck for good leaks one goroutine per run,
at most one per `cacheFor`.

`Check` panics on an empty name, a name already registered, a nil function, or a
call after the application has started.

### Details

With `details: true`, readiness answers JSON naming each check, sorted by name:

```json
{"status":"unavailable","checks":{"cache":"ok","db":"failing"}}
```

While draining the checks are not run, so the body is `{"status":"draining"}`;
with no checks registered it is `{"status":"ok"}`. A probe that stops waiting for
another probe's run, because its own request ended, gets `{"status":"unavailable"}`
with no checks. The status code is the same as
without `details`. The names are your own, so leave `details` off when the
readiness endpoint is reachable from outside and the names say more than you want.

## Configuration

```json
{
  "elagoht/health": {
    "livePath": "/healthz",
    "readyPath": "/readyz",
    "checkTimeout": "2s",
    "cacheFor": "1s",
    "details": false,
    "maxInFlight": 0
  }
}
```

Or from Go, as the defaults configuration starts from:

```go
health.NewWith(health.Options{LivePath: "/live", ReadyPath: "/ready"})
```

| Option | JSON | Default | |
| --- | --- | --- | --- |
| `LivePath` | `livePath` | `"/healthz"` | The liveness endpoint |
| `ReadyPath` | `readyPath` | `"/readyz"` | The readiness endpoint |
| `CheckTimeout` | `checkTimeout` | `"2s"` | How long one check may run. A Go duration string, or nanoseconds |
| `CacheFor` | `cacheFor` | `"1s"` | How long one readiness result is reused. A Go duration string, or nanoseconds |
| `Details` | `details` | `false` | A JSON readiness body naming each check as `ok` or `failing` |
| `MaxInFlight` | `maxInFlight` | `0` | Requests served at once at most; `0` is no cap |

Startup fails when `livePath` equals `readyPath`, when either does not start with
`/`, when a page the application, or a plugin listed before health, registers at
either path, or when `checkTimeout`, `cacheFor` or `maxInFlight` is negative.

## Draining

On `SIGTERM`, or `app.Shutdown(ctx)`, collage first drains: it tells this plugin,
which turns readiness to `503 draining`, closes kept-alive connections after their
next response, and keeps serving for `ServerConfig.DrainDelay`. Only then does it
stop taking connections. Set `DrainDelay` a little longer than the readiness
probe's period times its failure threshold, so the balancer has taken the instance
out before the port closes. It is ignored in development. See
[Deployment](https://collage.furkanbaytekin.dev/en/docs/deployment/) in the
collage docs.

## Load shedding

With `maxInFlight` above `0`, the plugin caps how many requests it lets through
at once. The request that would go past the cap is refused straight away:

```
HTTP/1.1 503 Service Unavailable
Retry-After: 1
Cache-Control: no-store
Content-Type: text/plain; charset=utf-8

overloaded
```

- **One global cap.** The count covers the whole process, not each client or
  path. Use ratelimit for a limit per client.
- **No queue.** A refused request is not held until a slot frees up. It is
  answered `503` at once, whatever its method, and the client is told to retry in
  a second.
- **The probes are exempt.** `livePath` and `readyPath` are never counted or
  refused. A loaded instance still answers its readiness probe, and it still
  says `draining` once a drain starts.
- **Build captures are exempt.** A static build asks the handler for each page
  to capture its headers (`collage.IsCapture`). Those requests are not clients,
  so they are never counted or refused, even while live requests fill the cap.
- **A slot is held until the response is done.** It is given back when the
  handler returns, even when the handler panics. A long-lived response, such as
  a stream, keeps its slot for as long as it runs.

Only requests that reach the plugin's middleware are counted. Listed first, that
is every page, document, mount and handler request. Collage answers a few before
any middleware runs, and those are not counted: the development reload channel,
and a request whose path needs cleaning, which gets a redirect or 404.
`collage.RequestHook` plugins such as accesslog still see a refused request.

## Kubernetes

```yaml
containers:
  - name: site
    readinessProbe:
      httpGet: { path: /readyz, port: 3000 }
      periodSeconds: 2
      failureThreshold: 1
    livenessProbe:
      httpGet: { path: /healthz, port: 3000 }
      periodSeconds: 10
      failureThreshold: 3
terminationGracePeriodSeconds: 30
```

With these probes, `DrainDelay: 5 * time.Second` covers the readiness probe's two
seconds with room to spare. Keep `terminationGracePeriodSeconds` above
`DrainDelay + ShutdownTimeout`, or the pod is killed mid-shutdown.
