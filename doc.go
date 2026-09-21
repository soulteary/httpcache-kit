// Package httpcache is an RFC 7234 HTTP cache for Go: freshness and heuristic
// expiration, conditional revalidation, Vary-aware cache keys, invalidation on
// unsafe methods, and memory, disk and VFS backends behind one [Cache]
// interface.
//
// # Layout
//
// The root package does not import a metrics backend. It defines [Metrics] --
// what the cache records -- and defaults to [NopMetrics], so a service that
// never exports metrics does not link Prometheus:
//
//   - github.com/soulteary/httpcache-kit/v4/prometheusmetrics -- records into
//     Prometheus via metrics-kit, and with it protobuf.
//
// Measured for a program importing only the root package, v3.0.0 against
// v4.0.0: 51 fewer linked packages, 17 fewer modules and a 32.5% smaller
// binary. A program that does use the subpackage pays 0.8% more than it did
// on v3 -- the cost is deferred, not removed, and only to those who want it.
//
// Logging is not split the same way. The handler logs on paths the cache
// cannot report any other way, so a logger is not optional in the way a
// metrics exporter is; logger-kit accounts for 5 of the remaining packages.
//
// # Getting started
//
//	cache := httpcache.NewMemoryCache()
//	handler := httpcache.NewHandler(cache, upstream)
//	handler.Shared = true // a reverse proxy in front of many users
//
//	http.ListenAndServe(":8080", handler)
//
// A shared cache refuses to store responses marked private and strips private
// headers; a private cache does not. Getting this backwards serves one user's
// response to another, so [NewSharedHandler] exists to make the choice
// explicit rather than a field somebody forgets to set.
//
// # Recording metrics
//
// Install a recorder once, and every cache and handler in the process reports
// through it:
//
//	registry := metrics.NewRegistry("myproxy")
//	prometheusmetrics.New(registry) // installs itself as the default
//
// To record somewhere else -- OpenTelemetry, statsd, a test double --
// implement [Metrics]. Embed [NopMetrics] to inherit no-ops for the methods
// you do not need, so a method added in a later release cannot break your
// implementation.
package httpcache
