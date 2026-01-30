# httpcache-kit

RFC7234-compliant HTTP cache handler for Go, with memory and disk backends (via [vfs-kit](https://github.com/soulteary/vfs-kit)), Cache-Control parsing, and Prometheus metrics. Evolved from [lox/httpcache](https://github.com/lox/httpcache) (MIT).

## Installation

```bash
go get github.com/soulteary/httpcache-kit
```

## Example

```go
package main

import (
	"log"
	"net/http"
	"net/http/httputil"

	httpcache "github.com/soulteary/httpcache-kit"
)

func main() {
	proxy := &httputil.ReverseProxy{
		Director: func(r *http.Request) {},
	}

	handler := httpcache.NewHandler(httpcache.NewMemoryCache(), proxy)
	handler.Shared = true

	log.Print("proxy listening on http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", handler))
}
```

Disk-backed cache with config:

```go
	cache, err := httpcache.NewDiskCacheWithConfig("/var/cache/myproxy", httpcache.DefaultCacheConfig())
	if err != nil {
		log.Fatal(err)
	}
	defer cache.Close()

	handler := httpcache.NewHandlerWithOptions(cache, proxy, &httpcache.HandlerOptions{Logger: myLogger})
```

## Features

- RFC7234-compliant caching (with documented caveats)
- Memory and disk storage (disk uses vfs-kit for VFS abstraction)
- Configurable TTL, max size, cleanup interval, LRU eviction
- Optional Prometheus metrics via metrics-kit
- Optional debug logging via logger-kit

## Caveats

- Conditional requests (e.g. `Range`) are not cached.

## References

- [RFC 7234](http://httpwg.github.io/specs/rfc7234.html) – Caching
- [lox/httpcache](https://github.com/lox/httpcache) – Original HTTP caching library (MIT)
