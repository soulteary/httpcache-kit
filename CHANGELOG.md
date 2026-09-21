# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
Because Go encodes the major version in the import path, every major release
also changes the module path. The current one is
`github.com/soulteary/httpcache-kit/v4`.

## [Unreleased]

## [4.0.0] — 2026-09-21

### Changed — BREAKING

- **Prometheus moved to the `prometheusmetrics` subpackage.** The root package
  no longer imports `prometheus/client_golang`, so a service that never
  exports metrics no longer links it — or protobuf. Measured for a program
  importing only the root package, built `-trimpath` against v3.0.0 and
  v4.0.0:

  | | v3.0.0 | v4.0.0 |
  |---|---|---|
  | binary | 10,789,883 B | 7,284,398 B (**−32.5%**) |
  | linked packages | 281 | 230 (−51) |
  | modules in `go.mod` | 52 | 35 (−17) |
  | lines in `go.sum` | 46 | 36 (−10) |
  | Prometheus/protobuf packages | 41 | **0** |

  A program that *does* record metrics pays 10,880,440 B — 0.8% more than it
  did on v3, for the interface indirection and one extra package. The cost is
  deferred to the services that want it, not removed.

  | Removed from the root package | Replacement |
  |---|---|
  | `httpcache.CacheMetrics` (struct) | `prometheusmetrics.Metrics` |
  | `httpcache.NewCacheMetrics` | `prometheusmetrics.New` |

  `httpcache.Metrics` now names the *interface* the cache records through, not
  a Prometheus struct. Keeping a deprecated alias was not an option, for the
  reason health-kit could not keep one when it split its Redis probe: an alias
  has to import `prometheus/client_golang`, which relinks it and gives back
  the entire benefit.

  **The module path is therefore now `github.com/soulteary/httpcache-kit/v4`**,
  by the import compatibility rule. Every user must update the import path,
  including services with no metrics at all, which are otherwise unaffected.

- **`GetDefaultMetrics` never returns nil.** It returned `*CacheMetrics`,
  which was nil until something registered metrics; nil-receiver methods made
  that safe. An interface has no such courtesy — calling a method on a nil
  interface panics — so "unset" is now a real object, [`NopMetrics`]. Code
  written as `if httpcache.GetDefaultMetrics() != nil` will now always take
  the true branch; delete the check. `SetDefaultMetrics(nil)` installs
  `NopMetrics` rather than arming a nil.

- **`Handler.SetMetrics` takes the interface**, and likewise treats nil as
  `NopMetrics`.

- Nothing else changed. Cache behaviour, the handler, the backends, cache keys,
  invalidation and the recorded metric names, labels and buckets are all
  identical — the Prometheus constructors were moved verbatim.

### Added

- The `prometheusmetrics` subpackage: `prometheusmetrics.New(registry)` builds
  the collectors, installs itself as the process-wide recorder and returns it,
  exactly as `NewCacheMetrics` did.
- **`Metrics`**, the interface, and **`NopMetrics`**, the no-op default. Embed
  `NopMetrics` to implement only the methods you care about, so a method added
  in a later minor release cannot break your recorder. Recording into
  OpenTelemetry, statsd or a test double no longer requires Prometheus.
- A package doc in `doc.go` describing the layout, the shared/private
  distinction and how to record metrics.
- `.github/workflows/release.yml`. Three major versions have been tagged with
  nothing checking any of them, and a tag whose major version disagrees with
  the module path is unfetchable — caught at tag time or not at all. It runs
  the CI gate against the tagged commit plus two checks that only matter at
  tag time: the module path must carry the tag's major version (v0 and v1
  taking no suffix), and both READMEs' `go get` line must name that path.
- `CHANGELOG.md` — this file.

### Fixed

- **A handler can no longer panic on a missing recorder.** Every hazard the
  interface introduces is the same one: what used to be a nil `*CacheMetrics`
  with harmless nil-receiver methods is now a nil interface that panics on
  first use. `Handler` records through an internal accessor that falls back to
  `NopMetrics`, matching the `logRef` accessor the package already used for
  its logger, so a zero-value `Handler`, a `SetMetrics(nil)` and an
  unregistered default are all safe. Each is covered by a test that fails
  with a nil dereference if the guard is removed.
- **The cache's own tests no longer need Prometheus.** `TestEvictIfNeeded_WithMetrics`
  and `TestCleanup_WithMetrics` had to build real collectors to exercise the
  recording paths, and then asserted nothing about them — they logged
  "eviction may have run". They now use an in-package counting recorder and
  assert the call actually happened. That matters here specifically: this
  release removed the `if getDefaultMetrics() != nil` guards those calls sat
  behind, and nothing would have noticed if a call had been dropped with them.
