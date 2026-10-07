// A collage plugin that answers liveness and readiness probes, turns readiness
// false while the server drains, runs the application's checks, and can shed
// load past a cap on requests in flight.
//
// It requires collage the way any consumer does.
module github.com/Elagoht/collage-health

go 1.26

require github.com/Elagoht/collage v0.52.0

replace github.com/Elagoht/collage => ../collage-health-core
