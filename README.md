# httpcache-kit

[![Go Reference](https://pkg.go.dev/badge/github.com/soulteary/httpcache-kit/v3.svg)](https://pkg.go.dev/github.com/soulteary/httpcache-kit/v3)
[![Go Report Card](.github/goreportcard.svg)](.github/goreportcard-report.md)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

[中文文档](README_CN.md)

An RFC 7234-compliant HTTP cache handler for Go. Wraps any `http.Handler` —
typically a `httputil.ReverseProxy` — with memory or disk storage, Cache-Control
parsing, conditional revalidation, RFC 7234 invalidation and optional Prometheus
metrics.

Evolved from [lox/httpcache](https://github.com/lox/httpcache) (MIT).

## Features

- **RFC 7234 caching**: freshness, heuristic expiration, revalidation, `Vary`
- **Invalidation**: an unsafe method invalidates the request URI and the URIs named by the response's `Location` / `Content-Location`
- **Private and shared profiles**: a shared cache refuses `private` responses and strips `private` headers
- **Memory, disk and VFS backends**: disk storage goes through [vfs-kit](https://github.com/soulteary/vfs-kit)
- **Bounded**: configurable TTL, max size, cleanup interval, LRU eviction
- **Observable, and only if you ask**: Prometheus metrics live in the `prometheusmetrics` subpackage, so importing the root package does not link Prometheus — debug logging via [logger-kit](https://github.com/soulteary/logger-kit)
- **Graceful shutdown**: background cache writes are tracked and can be awaited

## Requirements

- **Go 1.27+** (`go.mod` declares `go 1.27.0`)
- `github.com/prometheus/client_golang` **only if you import `prometheusmetrics`**

The root package depends on `logger-kit/v3` and `vfs-kit`. Prometheus and
`metrics-kit/v3` are reached only through the `prometheusmetrics` subpackage,
so a service that does not export metrics links neither — 51 fewer packages
and a 32.5% smaller binary than v3. Applications written against the
`logger-kit/v2` or `metrics-kit/v2` types should remain on
`github.com/soulteary/httpcache-kit/v2`, and applications still on the v1 kit
ecosystem on `github.com/soulteary/httpcache-kit` v1.

## Installation

```bash
go get github.com/soulteary/httpcache-kit/v4
```

## Quick Start

### Shared cache (reverse proxy)

```go
package main

import (
    "log"
    "net/http"
    "net/http/httputil"

    httpcache "github.com/soulteary/httpcache-kit/v3"
)

func main() {
    proxy := &httputil.ReverseProxy{
        Director: func(r *http.Request) {},
    }

    // NewSharedHandler, not NewHandler: this cache serves more than one user.
    handler := httpcache.NewSharedHandler(httpcache.NewMemoryCache(), proxy)

    log.Print("proxy listening on http://localhost:8080")
    log.Fatal(http.ListenAndServe(":8080", handler))
}
```

### Private cache (single user)

```go
handler := httpcache.NewHandler(httpcache.NewMemoryCache(), upstream)
```

### Disk-backed, with options

```go
cache, err := httpcache.NewDiskCacheWithConfig("/var/cache/myproxy",
    httpcache.DefaultCacheConfig().
        WithMaxSize(2 * 1024 * 1024 * 1024).
        WithTTL(24 * time.Hour).
        WithCleanupInterval(30 * time.Minute))
if err != nil {
    log.Fatal(err)
}
defer cache.Close()

handler := httpcache.NewHandlerWithOptions(cache, proxy, &httpcache.HandlerOptions{
    Logger: myLogger,
})
handler.Shared = true
```

## Shared vs Private

This is the one decision to get right, because the unsafe default is the one a
field can be forgotten in.

| | `NewHandler` | `NewSharedHandler` |
|---|---|---|
| `Handler.Shared` | `false` | `true` |
| Correct for | a per-user cache | a reverse proxy, any cache serving more than one user |
| `Cache-Control: private` | stored | **not stored** |
| Responses to authorized requests | stored | **not stored** unless marked `public` or carrying `s-maxage` |
| Headers listed in `private` | kept | **stripped before storing** |
| `s-maxage` | ignored | honoured over `max-age` |

A shared cache running with `Shared: false` will serve one user's private
response to another. Prefer the constructor over setting the field — a
constructor cannot be forgotten.

## Backends

```go
httpcache.NewMemoryCache()                              // Cache
httpcache.NewMemoryCacheWithConfig(cfg)                 // ExtendedCache
httpcache.NewDiskCache("/var/cache/x")                  // Cache
httpcache.NewDiskCacheWithConfig("/var/cache/x", cfg)   // ExtendedCache
httpcache.NewVFSCache(fs)                               // Cache
httpcache.NewVFSCacheWithConfig(fs, cfg)                // ExtendedCache
```

`Cache` is the minimum the handler needs:

```go
type Cache interface {
    Header(key string) (Header, error)
    Retrieve(key string) (*Resource, error)
    Store(res *Resource, keys ...string) error
    Freshen(res *Resource, keys ...string) error
    Invalidate(keys ...string)
}
```

`ExtendedCache` adds management, and is what the `*WithConfig` constructors
return:

```go
type ExtendedCache interface {
    Cache
    Stats() CacheStats
    Cleanup() CleanupResult
    Purge() error
    Close() error
}
```

```go
stats := cache.Stats()
log.Printf("items=%d bytes=%d hits=%d misses=%d stale=%d",
    stats.ItemCount, stats.TotalSize, stats.HitCount, stats.MissCount, stats.StaleCount)

result := cache.Cleanup()
log.Printf("removed %d items (%d bytes, %d stale markers) in %s",
    result.RemovedItems, result.RemovedBytes, result.RemovedStaleEntries, result.Duration)
```

`Retrieve` returns `ErrNotFoundInCache` for a miss — match it with `errors.Is`.
A `*Resource` it returns owns a file handle on the disk backend, so close it.

### On-disk layout

The disk backend keeps four things under its directory:

```
body/v1/<hashed-key>      response body
header/v1/<hashed-key>    status line, headers, and the store timestamp
staging/v1/               entries being written, empty when idle
stale-markers.json        invalidation state
```

An entry is its body plus its header, and the two are published together by
rename, so a reader sees the whole previous entry or the whole new one. That
holds **between goroutines sharing one live cache**, and no further:

- The two renames are sequential. A process that exits between them leaves the
  new body beside the old header, and startup removes the leftover staging file
  rather than repairing the pair.
- The lock is per-instance. Two caches opened on one directory do not
  coordinate, and their publications can interleave.

Nothing under `staging/v1` is a cache entry: it is never scanned, and whatever
an interrupted process left there is removed at startup.

Only `body/v1` and `header/v1` count toward `MaxSize` and `Stats().TotalSize`.
The other `Stats()` fields are independent of these directories —
`StaleCount` tracks `stale-markers.json`, and `HitCount`/`MissCount` are
counters.

## Configuration

```go
cfg := httpcache.DefaultCacheConfig().
    WithMaxSize(10 * 1024 * 1024 * 1024).
    WithTTL(7 * 24 * time.Hour).
    WithCleanupInterval(1 * time.Hour).
    WithStaleMapTTL(24 * time.Hour).
    Validate()
```

| Option | Default | Notes |
|--------|---------|-------|
| `MaxSize` | `DefaultMaxCacheSize` (10 GiB) | `0` means unbounded; LRU eviction above it |
| `TTL` | `DefaultCacheTTL` (7 days) | `0` means no TTL |
| `CleanupInterval` | `DefaultCleanupInterval` (1 hour) | `0` disables the background cycle |
| `StaleMapTTL` | `DefaultStaleMapTTL` (24 hours) | how long the backend remembers a stale marker |

`Validate()` clamps negatives to zero and restores `StaleMapTTL` to its default
if it is non-positive. It mutates and returns the same config, so it chains.

### Handler options

```go
handler := httpcache.NewHandlerWithOptions(cache, upstream, &httpcache.HandlerOptions{
    Logger:         myLogger,          // *logger.Logger; nil uses the package logger
    StaleMarkerTTL: 7 * 24 * time.Hour,
})
```

`StaleMarkerTTL` is how long the **handler** remembers an invalidation for a
`Cache` that does not track staleness itself. It must be at least as long as
that cache retains entries: the marker is the only record that older entries are
pre-mutation, so expiring it first republishes them as fresh. Zero means
`DefaultCacheTTL`. A cache that keeps its own record ignores this.

## Cache Keys and Vary

A key is derived from the **effective request URI** and the method:

```go
key := httpcache.NewRequestKey(r)           // from a request
key = httpcache.NewKey("GET", u, r.Header)  // explicitly
key = key.ForMethod("HEAD")                 // the sibling key for another method
key = key.Vary(resp.Header.Get("Vary"), r)  // the variant key
keyString := key.String()
```

The request's own `Content-Location` does **not** affect the key. RFC 7234 uses
`Content-Location`, but the *response's*, and only for invalidation — letting a
request choose its key allows a client to park its response under another URL's
key, or read another URL's entry.

`Key.String()` is injective: the `Vary` section uses a control-byte separator
with each value quoted, and since `url.URL.String` percent-encodes control bytes
while `strconv.Quote` escapes them, neither side of the boundary can contain the
delimiter. Keys without `Vary` are plain and unchanged.

## Invalidation

An unsafe method (`POST`, `PUT`, `DELETE`, `PATCH`) invalidates, per RFC 7234
section 4.4:

- the effective request URI,
- the URI in the response's `Location` header,
- the URI in the response's `Content-Location` header,

for both the `GET` and `HEAD` keys. Cross-origin targets are ignored, so a
response cannot evict another origin's entries.

## Metrics

Metrics are opt-in at the *import* level: the root package knows what to
record, not where to send it. Pull in `prometheusmetrics` and you get
Prometheus; leave it out and you do not link Prometheus at all.

```go
import (
    metrics "github.com/soulteary/metrics-kit/v3"

    "github.com/soulteary/httpcache-kit/v4/prometheusmetrics"
)

registry := metrics.NewRegistry("myproxy")
m := prometheusmetrics.New(registry) // also installs itself as the default
handler.SetMetrics(m)

// The process-wide default, which every cache and handler reports through
httpcache.SetDefaultMetrics(m)
m = httpcache.GetDefaultMetrics()

// Feed gauges from a cache's own view
m.UpdateCacheStats(cache.Stats())
```

It exposes hits, misses, skips, evictions, store and retrieve operations,
item count, size in bytes, stale count, cleanup duration, and upstream
duration and errors.

To record somewhere else — OpenTelemetry, statsd, a test double — implement
`httpcache.Metrics`. Embed `httpcache.NopMetrics` to inherit no-ops for the
methods you do not need, so a method added in a later release cannot break
your recorder:

```go
type hitCounter struct {
    httpcache.NopMetrics
    hits atomic.Int64
}

func (c *hitCounter) RecordCacheHit(string) { c.hits.Add(1) }
```

`GetDefaultMetrics` never returns nil — until something calls
`SetDefaultMetrics`, it is a `NopMetrics`.

## Logging

```go
import logger "github.com/soulteary/logger-kit/v3"

httpcache.SetLogger(myLogger)     // package-level logger
httpcache.SetDebugLogging(true)   // verbose cache decisions
on := httpcache.IsDebugLogging()
```

## Response Headers

| Header | Values | Meaning |
|--------|--------|---------|
| `X-Cache` | `HIT` | served from cache |
| | `MISS` | fetched from upstream and stored |
| | `SKIP` | not cacheable, or cache bypassed |
| `Proxy-Date` | HTTP-date | when this cache received the response |

## Cache-Control

```go
cc, err := httpcache.ParseCacheControl("max-age=3600, s-maxage=60, private")
cc, err = httpcache.ParseCacheControlHeaders(resp.Header)

cc.Has("no-store")
value, ok := cc.Get("max-age")
d, err := cc.Duration("max-age")
cc.Add("stale-while-revalidate", "30")
header := cc.String()
```

## Resources

```go
res := httpcache.NewResourceBytes(200, body, header)
res = httpcache.NewResource(200, readSeekCloser, header)

res.Status()
res.Header()
res.Age()
res.Expires()
res.MaxAge(shared)
res.HasExplicitExpiration()
res.HeuristicFreshness()
res.HasValidators()
res.MustValidate(shared)
res.IsStale()
res.MarkStale()
res.LastModified()
res.RemovePrivateHeaders()
res.Via()
```

## Graceful Shutdown

The handler stores responses in the background. Await them before closing the
backend, or an in-flight write hits a closed cache:

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

if err := handler.Shutdown(ctx); err != nil {
    log.Printf("cache shutdown: %v", err)
}
cache.Close()
```

`httpcache.Writes` is a package-level `sync.WaitGroup` covering every handler's
background writes, for tests that need to wait on all of them.

## Caveats

- Conditional requests carrying `Range` are not cached.
- `Clock` is a package-level variable, swappable in tests.

## Changelog

Release-by-release detail, with the measured numbers behind each claim, lives
in [CHANGELOG.md](CHANGELOG.md).

## Upgrade Notes (v4.0.0)

**Prometheus moved to the `prometheusmetrics` subpackage**, so the root
package no longer links it. If your service does not export metrics, the only
change you make is the import path.

### What this buys you

Measured for a program importing only the root package, built `-trimpath`
against v3.0.0 and v4.0.0:

| | v3.0.0 | v4.0.0 |
|---|---|---|
| binary | 10,789,883 B | 7,284,398 B (**−32.5%**) |
| linked packages | 281 | 230 (−51) |
| modules in `go.mod` | 52 | 35 (−17) |
| lines in `go.sum` | 46 | 36 (−10) |
| Prometheus/protobuf packages | 41 | **0** |

A program that *does* record metrics pays 0.8% more than on v3, for the
interface indirection and one extra package. The cost is deferred to the
services that want it, not removed.

### What you have to change

1. **The import path**, everywhere:

   ```diff
   -go get github.com/soulteary/httpcache-kit/v3
   +go get github.com/soulteary/httpcache-kit/v4
   ```

2. **If you use metrics**, import the subpackage and rename two identifiers:

   | v3 | v4 |
   |---|---|
   | `httpcache.CacheMetrics` (struct) | `prometheusmetrics.Metrics` |
   | `httpcache.NewCacheMetrics(reg)` | `prometheusmetrics.New(reg)` |

   ```diff
   +import "github.com/soulteary/httpcache-kit/v4/prometheusmetrics"

   -m := httpcache.NewCacheMetrics(registry)
   +m := prometheusmetrics.New(registry)
    handler.SetMetrics(m)
   ```

   `httpcache.Metrics` is now the *interface* the cache records through, not a
   Prometheus struct. `SetDefaultMetrics`, `GetDefaultMetrics` and
   `Handler.SetMetrics` keep their names and take it.

   There is no deprecated alias, deliberately: an alias would have to import
   `prometheus/client_golang`, which relinks it and gives back the whole
   benefit.

3. **Delete any nil check on `GetDefaultMetrics`.** It used to return a
   `*CacheMetrics` that was nil until metrics were registered, and nil-receiver
   methods made that safe. An interface has no such courtesy — a method call on
   a nil interface panics — so "unset" is now a real object, `NopMetrics`:

   ```diff
   -if m := httpcache.GetDefaultMetrics(); m != nil {
   -    m.RecordCacheHit("GET")
   -}
   +httpcache.GetDefaultMetrics().RecordCacheHit("GET")
   ```

   `SetDefaultMetrics(nil)` installs `NopMetrics` rather than arming a nil.

Cache behaviour, the handler, the backends, cache keys, invalidation and the
recorded metric names, labels and buckets are all unchanged — the Prometheus
constructors were moved verbatim.

### Why logging was not split the same way

`logger-kit` accounts for 5 of the remaining packages, and the handler logs on
paths the cache cannot report any other way. A logger is not optional the way
a metrics exporter is, so splitting it would cost an interface and an import
for almost nothing.

## Upgrade Notes (v3.0.0)

The cache's own API did not change. What changed is the module path — this
module's and two of its dependencies' — because `logger-kit` and `metrics-kit`
went to `/v3`, and the cache API hands you their types.

1. **Change the module path.** Every import, in every file:

   ```bash
   go get github.com/soulteary/httpcache-kit/v3
   go mod edit -droprequire github.com/soulteary/httpcache-kit/v2
   ```

   ```diff
   -httpcache "github.com/soulteary/httpcache-kit/v2"
   +httpcache "github.com/soulteary/httpcache-kit/v3"
   ```

   `go get -u` will not do this for you; v2 stays on `v2.5.0`.

2. **Re-point `logger-kit` and `metrics-kit` too, if you name their types.**
   `SetLogger`, `HandlerOptions.Logger` and `NewCacheMetrics` take
   `*logger.Logger` and `*metrics.Registry`, and a v2 type does not satisfy a v3
   parameter — the module path is part of the type's identity. This is the only
   thing that can fail to compile:

   ```diff
   -logger "github.com/soulteary/logger-kit/v2"
   -metrics "github.com/soulteary/metrics-kit/v2"
   +logger "github.com/soulteary/logger-kit/v3"
   +metrics "github.com/soulteary/metrics-kit/v3"
   ```

   Every name this cache uses from them — `logger.Default`, `logger.NewDefault`,
   `logger.Middleware`, `logger.MiddlewareConfig`, `metrics.NewRegistry`,
   `metrics.Registry`, `metrics.HTTPDurationBuckets` — kept its signature. If
   you used a `FiberHandler`, a `NewFiberMiddleware` or a `SkipFuncFiber` field
   from either kit, those moved to their `fiberadapter` subpackages; see those
   kits' own v3 notes.

3. **Nothing else.** No name in this package was added, removed or changed. Once
   the imports compile, you are done.

### What this buys you

Those kits moved their Fiber support into `fiberadapter` subpackages, so their
root packages no longer link a web framework — and this cache never used Fiber
in the first place. It was carrying the framework because `logger-kit/v2` and
`metrics-kit/v2` reached it:

| | v2.5.0 | v3.0.0 |
| --- | --- | --- |
| Fiber/fasthttp/compress/msgp packages linked | 39 | **0** |
| packages the library links | 340 | 280 |
| modules in the build list | 61 | 51 |
| `// indirect` lines in `go.mod` | 24 | 12 |

The twelve dropped requirements are `gofiber/fiber/v3`, `gofiber/schema`,
`gofiber/utils/v2`, `klauspost/compress`, `molecule-man/go-brrr`,
`philhofer/fwd`, `tinylib/msgp`, `valyala/bytebufferpool`, `valyala/fasthttp`,
`golang.org/x/crypto`, `golang.org/x/net` and `golang.org/x/text`. Fiber still
appears in `go list -m all`, because `logger-kit/v3` and `metrics-kit/v3`
require it for their own `fiberadapter` subpackages — but no package from it is
compiled into a binary that uses this cache.

If you serve this cache behind Fiber, nothing is lost: you were reaching Fiber
through your own import, not through this module.

### `vfs-kit` v1.4.0 → v1.4.2

Data-race fixes in the in-memory filesystem, which is what `NewMemoryCache` and
`NewMemoryCacheWithConfig` run on. The single filesystem-wide mutex became
per-directory locking, and `File.FileMode` and the compressed-read path now take
the read lock they were missing. No API of it that this cache uses changed;
v1.4.2 also adds an exported `ErrRemoveRoot`, which this cache cannot produce —
it only ever removes individual entry files, never a filesystem root.

## Upgrade Notes (v2.5.0)

No API was added, removed or changed. The disk backend writes entries
differently, and there is a new directory inside the cache directory.

- **Entries are published by rename on the disk backend.** The previous write
  opened the target with `O_CREATE|O_TRUNC`, so new contents were never visible
  as a unit: on a first store an empty file appeared before any bytes reached
  it, and on a re-store a perfectly readable entry was emptied for as long as
  the copy took. Replacing a 128 KiB entry under eight concurrent readers
  produced 2 misses and 258 reads that saw neither the old nor the new body in
  full; a watcher caught the header file empty 4543 times across 200 re-stores.
  A reader sharing the cache now sees the whole previous entry or the whole new
  one — see the on-disk layout section for the two limits on that, a crash
  between the two renames and two instances on one directory.
- **A body and its header change together.** `Retrieve` opens the body first and
  reads the header second, so a publish landing between those two steps used to
  hand back one version's payload with the other's status and headers — a
  `Content-Length` or `Content-Encoding` describing a body that was no longer
  there. Under load, 148 of 600 retrievals mismatched. Both files are now
  renamed under one lock that `Retrieve` holds across both lookups.
- **`staging/v1` is new inside the cache directory.** Entries are written there
  before being renamed into place. It is outside everything that is scanned, so
  nothing in it is ever mistaken for an entry, and it is swept at startup — a
  process killed mid-write leaves a file behind that nothing else would reclaim.
  **If you size, back up or rsync the cache directory, include it**; it is empty
  when the cache is idle.
- **Storing costs more, mostly on small entries.** Measured per store against
  v2.4.0: 4 KiB went 193µs → 967µs, 2 MiB went 1.66ms → 2.03ms. Large entries
  are close to free; the small-entry cost is the extra syscalls against a very
  short write. Numbers are from a container filesystem and will differ
  elsewhere.
- **Entry files are not fsynced.** Rename is what makes publication atomic;
  durability across a crash is not what a cache needs, and an entry lost that
  way is a miss. `stale-markers.json` is still fsynced, because losing
  invalidation state would republish superseded content.
- **Memory and other VFS backends are unchanged.** The `VFS` interface has no
  rename, so they keep the in-place write. The header-completeness check added
  in v2.4.0 still turns that window into a miss for them.

## Upgrade Notes (v2.2.0)

This release changes which entries are served and how they are keyed. One
constructor and one option were added; nothing was removed.

- **Invalidation now happens.** `invalidateResource()`'s entire body was a debug
  log call, so a resource that had been `POST`ed to, `PUT` or `DELETE`d kept
  being served from cache until its own freshness lifetime expired. It now
  invalidates the request URI and the same-origin URIs named by the response's
  `Location` and `Content-Location`, for both the `GET` and `HEAD` keys. **Expect
  more upstream traffic after unsafe methods** — that is the bug being fixed.
- **A request's `Content-Location` no longer selects the cache key.** It did,
  while the upstream request still used the original URL, so a client could park
  its own response under a different URL's key (shared cache poisoning) or read
  another URL's entry. If you deliberately relied on that to alias entries, there
  is no replacement — it was not a feature.
- **`Key.String()` changed for keys with `Vary`.** It joined a raw URL and raw
  header values with `":"` and `"::"`, so a crafted URL could collide with a
  different URL carrying `Vary` values. The `Vary` section now uses a quoted,
  control-byte-separated encoding. **Existing cached entries with `Vary` will
  miss once** and be re-fetched; keys without `Vary` are unchanged.
- **`NewSharedHandler` is the constructor for a reverse proxy.** `Shared`
  defaults to `false`, which is the correct private-cache profile and the wrong
  one for a shared cache — and a field can be forgotten in a way a constructor
  cannot. If you set `handler.Shared = true` by hand, nothing breaks; new code
  should use the constructor.
- **The disk backend no longer leaks a file handle per `Vary` lookup.** The
  primary `Resource` was overwritten without being closed.
- **`CacheControl.String()` no longer emits empty entries.** It allocated its key
  slice with `make([]string, len(cc))` and then appended, so the output began with
  `len(cc)` empty fields.
- **`HandlerOptions.StaleMarkerTTL` is new.** Set it to at least your cache's
  retention when the cache does not track staleness itself; otherwise an expired
  marker republishes pre-mutation entries as fresh.

## Testing

```bash
go test ./...

# With coverage
go test ./... -coverprofile=coverage.out -covermode=atomic
go tool cover -func=coverage.out
```

## References

- [RFC 7234](http://httpwg.github.io/specs/rfc7234.html) — HTTP/1.1 Caching
- [lox/httpcache](https://github.com/lox/httpcache) — the original library (MIT)

## License

Apache License 2.0 — see [LICENSE](LICENSE). Portions derive from
[lox/httpcache](https://github.com/lox/httpcache), MIT licensed.
