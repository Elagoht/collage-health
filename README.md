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

## Endpoints

| Path | Answers |
| --- | --- |
| `/healthz` (`livePath`) | `200 ok` whenever the process serves a request. It runs no checks: restarting a process whose database is down does not bring the database back |
| `/readyz` (`readyPath`) | `200 ok`, or `503 draining` once a drain has started |

Both answer `GET` and `HEAD` only, as `text/plain; charset=utf-8` with
`Cache-Control: no-store`. Any other method gets `405` with `Allow: GET, HEAD`.

The middleware answers before routing, so the paths belong to the plugin:

- A page registered at either path fails startup, with an error naming the page.
- A document or a handler at either path is not checked, and is shadowed: the
  middleware answers and it is never reached. A project scaffolded with the demos
  has a `/healthz` document; delete it, or move the liveness endpoint with
  `livePath`.

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
| `Details` | `details` | `false` | A JSON body naming each check |
| `MaxInFlight` | `maxInFlight` | `0` | Requests served at once at most; `0` is no cap |

Startup fails when `livePath` equals `readyPath`, when either does not start with
`/`, when a page claims either, or when `checkTimeout`, `cacheFor` or `maxInFlight`
is negative.

## Draining

On `SIGTERM`, or `app.Shutdown(ctx)`, collage first drains: it tells this plugin,
which turns readiness to `503 draining`, closes kept-alive connections after their
next response, and keeps serving for `ServerConfig.DrainDelay`. Only then does it
stop taking connections. Set `DrainDelay` a little longer than the readiness
probe's period times its failure threshold, so the balancer has taken the instance
out before the port closes. It is ignored in development. See
[Deployment](https://collage.furkanbaytekin.dev/en/docs/deployment/) in the
collage docs.

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
