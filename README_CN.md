# httpcache-kit

[![Go Reference](https://pkg.go.dev/badge/github.com/soulteary/httpcache-kit/v3.svg)](https://pkg.go.dev/github.com/soulteary/httpcache-kit/v3)
[![Go Report Card](.github/goreportcard.svg)](.github/goreportcard-report.md)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

[English](README.md)

符合 RFC 7234 的 Go HTTP 缓存处理器。把任意 `http.Handler`——通常是
`httputil.ReverseProxy`——包一层，提供内存或磁盘存储、Cache-Control 解析、条件重校验、
RFC 7234 失效，以及可选的 Prometheus 指标。

源自 [lox/httpcache](https://github.com/lox/httpcache)（MIT）。

## 特性

- **RFC 7234 缓存**：新鲜度、启发式过期、重校验、`Vary`
- **失效**：非安全方法会使请求 URI，以及响应 `Location` / `Content-Location` 指向的 URI 失效
- **私有与共享两种模式**：共享缓存拒绝存储 `private` 响应，并剥掉 `private` 列出的 header
- **内存、磁盘与 VFS 后端**：磁盘存储经由 [vfs-kit](https://github.com/soulteary/vfs-kit)
- **有界**：可配置 TTL、最大容量、清理周期、LRU 淘汰
- **可观测，且按需付费**：Prometheus 指标放在 `prometheusmetrics` 子包，导入根包不会链接 Prometheus；调试日志通过 [logger-kit](https://github.com/soulteary/logger-kit) 输出
- **优雅关闭**：后台缓存写入被跟踪，可以等待其完成

## 要求

- **Go 1.27+**（`go.mod` 声明 `go 1.27.0`）
- **只有导入 `prometheusmetrics` 时**才需要 `github.com/prometheus/client_golang`

根包只依赖 `logger-kit/v3` 和 `vfs-kit`。Prometheus 和 `metrics-kit/v3` 只能经由
`prometheusmetrics` 子包到达，所以不暴露指标的服务两个都不会链接 —— 比 v3 少 51
个包，二进制小 32.5%。代码写的是
`logger-kit/v2` 或 `metrics-kit/v2` 类型的应用请继续使用
`github.com/soulteary/httpcache-kit/v2`；仍在 v1 kit 生态上的应用请继续使用
`github.com/soulteary/httpcache-kit` v1。

## 安装

```bash
go get github.com/soulteary/httpcache-kit/v4
```

## 快速开始

### 共享缓存（反向代理）

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

    // 用 NewSharedHandler，不是 NewHandler：这个缓存服务于多个用户。
    handler := httpcache.NewSharedHandler(httpcache.NewMemoryCache(), proxy)

    log.Print("proxy listening on http://localhost:8080")
    log.Fatal(http.ListenAndServe(":8080", handler))
}
```

### 私有缓存（单用户）

```go
handler := httpcache.NewHandler(httpcache.NewMemoryCache(), upstream)
```

### 磁盘后端 + 配置

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

## 共享与私有

这是唯一必须选对的决定，因为不安全的那个恰好是默认值，而字段是会被忘记设置的。

| | `NewHandler` | `NewSharedHandler` |
|---|---|---|
| `Handler.Shared` | `false` | `true` |
| 适用于 | 单用户缓存 | 反向代理，以及任何服务多个用户的缓存 |
| `Cache-Control: private` | 会存 | **不存** |
| 对已授权请求的响应 | 会存 | 除非标了 `public` 或带 `s-maxage`，否则**不存** |
| `private` 列出的 header | 保留 | 存储前**剥掉** |
| `s-maxage` | 忽略 | 优先于 `max-age` |

以 `Shared: false` 运行的共享缓存，会把一个用户的私有响应发给另一个用户。请优先使用
构造函数而不是手动设置字段——构造函数不会被忘记。

## 后端

```go
httpcache.NewMemoryCache()                              // Cache
httpcache.NewMemoryCacheWithConfig(cfg)                 // ExtendedCache
httpcache.NewDiskCache("/var/cache/x")                  // Cache
httpcache.NewDiskCacheWithConfig("/var/cache/x", cfg)   // ExtendedCache
httpcache.NewVFSCache(fs)                               // Cache
httpcache.NewVFSCacheWithConfig(fs, cfg)                // ExtendedCache
```

`Cache` 是处理器所需的最小接口：

```go
type Cache interface {
    Header(key string) (Header, error)
    Retrieve(key string) (*Resource, error)
    Store(res *Resource, keys ...string) error
    Freshen(res *Resource, keys ...string) error
    Invalidate(keys ...string)
}
```

`ExtendedCache` 增加了管理能力，也是 `*WithConfig` 系列构造函数的返回类型：

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

未命中时 `Retrieve` 返回 `ErrNotFoundInCache`——请用 `errors.Is` 判断。它返回的
`*Resource` 在磁盘后端上持有一个文件句柄，用完请关闭。

### 磁盘布局

磁盘后端在它的目录下保存四样东西：

```
body/v1/<hashed-key>      响应体
header/v1/<hashed-key>    状态行、header，以及写入时间戳
staging/v1/               正在写入的条目，空闲时为空
stale-markers.json        失效状态
```

一个条目由 body 和 header 共同构成，两者通过 rename 一并发布，因此读者看到的要么是完整的旧条目，要么是完整的新条目。这一点**只在共享同一个活动缓存实例的 goroutine 之间成立**，再往外不保证：

- 两次 rename 是顺序执行的。进程若在两者之间退出，磁盘上会留下新的 body 配旧的 header，而启动时只会清除残留的暂存文件，不会去修复这一对。
- 这把锁是实例级的。在同一个目录上打开的两个缓存实例之间没有协调，它们的发布可能交错。

`staging/v1` 下的任何东西都不是缓存条目：它不会被扫描，进程中断后残留在那里的文件会在启动时清除。

只有 `body/v1` 和 `header/v1` 计入 `MaxSize` 与 `Stats().TotalSize`。`Stats()` 的其他字段与这些目录无关 —— `StaleCount` 跟踪的是 `stale-markers.json`，`HitCount`/`MissCount` 则是计数器。

## 配置

```go
cfg := httpcache.DefaultCacheConfig().
    WithMaxSize(10 * 1024 * 1024 * 1024).
    WithTTL(7 * 24 * time.Hour).
    WithCleanupInterval(1 * time.Hour).
    WithStaleMapTTL(24 * time.Hour).
    Validate()
```

| 配置项 | 默认值 | 说明 |
|--------|--------|------|
| `MaxSize` | `DefaultMaxCacheSize`（10 GiB） | `0` 表示不限制；超过则按 LRU 淘汰 |
| `TTL` | `DefaultCacheTTL`（7 天） | `0` 表示没有 TTL |
| `CleanupInterval` | `DefaultCleanupInterval`（1 小时） | `0` 关闭后台清理周期 |
| `StaleMapTTL` | `DefaultStaleMapTTL`（24 小时） | 后端记住失效标记的时长 |

`Validate()` 会把负值归零，并在 `StaleMapTTL` 非正时恢复默认值。它原地修改并返回同一个
配置对象，因此可以链式调用。

### 处理器选项

```go
handler := httpcache.NewHandlerWithOptions(cache, upstream, &httpcache.HandlerOptions{
    Logger:         myLogger,          // *logger.Logger；nil 表示用包级 logger
    StaleMarkerTTL: 7 * 24 * time.Hour,
})
```

`StaleMarkerTTL` 是**处理器**为那些自身不记录失效状态的 `Cache` 保留失效标记的时长。
它必须不短于该缓存保留条目的时间：这个标记是"比它更老的条目都是变更前的数据"的唯一
记录，标记先过期就会把那些条目重新当作新鲜数据发出去。零表示 `DefaultCacheTTL`。
自己记录失效状态的缓存会忽略这个值。

## 缓存键与 Vary

键由**有效请求 URI** 和方法导出：

```go
key := httpcache.NewRequestKey(r)           // 来自请求
key = httpcache.NewKey("GET", u, r.Header)  // 显式构造
key = key.ForMethod("HEAD")                 // 另一方法的同级键
key = key.Vary(resp.Header.Get("Vary"), r)  // 变体键
keyString := key.String()
```

请求自身的 `Content-Location` **不会**影响键。RFC 7234 确实会用到
`Content-Location`，但用的是*响应*的，而且只用于失效——让请求自己挑键，意味着客户端
可以把自己的响应塞到别的 URL 的键下，或者读到别的 URL 的条目。

`Key.String()` 是单射的：`Vary` 部分采用控制字节作分隔符、每个值加引号，而
`url.URL.String` 会对控制字节做百分号编码、`strconv.Quote` 会转义它们，因此分界线
两侧都不可能含有该分隔符。不带 `Vary` 的键保持原样。

## 失效

非安全方法（`POST`、`PUT`、`DELETE`、`PATCH`）会按 RFC 7234 第 4.4 节使以下目标失效：

- 有效请求 URI，
- 响应 `Location` header 中的 URI，
- 响应 `Content-Location` header 中的 URI，

`GET` 和 `HEAD` 两个键都会处理。跨源目标会被忽略，因此一个响应无法清掉其他源的条目。

## 指标

指标是在 **import 层面**可选的：根包只知道该记录什么，不知道往哪里送。导入
`prometheusmetrics` 就得到 Prometheus；不导入就完全不链接 Prometheus。

```go
import (
    metrics "github.com/soulteary/metrics-kit/v3"

    "github.com/soulteary/httpcache-kit/v4/prometheusmetrics"
)

registry := metrics.NewRegistry("myproxy")
m := prometheusmetrics.New(registry) // 同时把自己装成默认记录器
handler.SetMetrics(m)

// 进程级默认值，所有 cache 和 handler 都经由它上报
httpcache.SetDefaultMetrics(m)
m = httpcache.GetDefaultMetrics()

// 用缓存自己的视图刷新 gauge
m.UpdateCacheStats(cache.Stats())
```

它暴露命中、未命中、跳过、淘汰、存取操作数、条目数、字节数、陈旧条目数、
清理耗时，以及上游耗时与错误。

要送到别处 —— OpenTelemetry、statsd、测试替身 —— 实现 `httpcache.Metrics` 即可。
嵌入 `httpcache.NopMetrics` 就能为用不到的方法继承空实现，这样以后版本新增方法
也不会让你的记录器编译失败：

```go
type hitCounter struct {
    httpcache.NopMetrics
    hits atomic.Int64
}

func (c *hitCounter) RecordCacheHit(string) { c.hits.Add(1) }
```

`GetDefaultMetrics` 永远不返回 nil —— 在调用 `SetDefaultMetrics` 之前，它是
`NopMetrics`。

## 日志

```go
import logger "github.com/soulteary/logger-kit/v3"

httpcache.SetLogger(myLogger)     // 包级 logger
httpcache.SetDebugLogging(true)   // 详细打印缓存决策
on := httpcache.IsDebugLogging()
```

## 响应 Header

| Header | 取值 | 含义 |
|--------|------|------|
| `X-Cache` | `HIT` | 由缓存响应 |
| | `MISS` | 回源取得并已存储 |
| | `SKIP` | 不可缓存，或绕过了缓存 |
| `Proxy-Date` | HTTP-date | 本缓存收到该响应的时间 |

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

## Resource

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

## 优雅关闭

处理器在后台存储响应。关闭后端之前请先等待这些写入完成，否则在途写入会撞上一个已关闭
的缓存：

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

if err := handler.Shutdown(ctx); err != nil {
    log.Printf("cache shutdown: %v", err)
}
cache.Close()
```

`httpcache.Writes` 是一个包级 `sync.WaitGroup`，覆盖所有处理器的后台写入，便于测试
等待全部完成。

## 注意事项

- 带 `Range` 的条件请求不会被缓存。
- `Clock` 是包级变量，测试中可以替换。

## 变更日志

逐版本的详细说明，以及每条结论背后的实测数字，见 [CHANGELOG.md](CHANGELOG.md)。

## 升级说明（v4.0.0）

**Prometheus 挪进了 `prometheusmetrics` 子包**，根包不再链接它。如果你的服务不暴露
指标，唯一要改的就是 import 路径。

### 这次升级换来了什么

实测一个只导入根包的程序，`-trimpath` 分别对 v3.0.0 和 v4.0.0 构建：

| | v3.0.0 | v4.0.0 |
|---|---|---|
| 二进制 | 10,789,883 B | 7,284,398 B（**−32.5%**） |
| 链接包 | 281 | 230（−51） |
| `go.mod` 模块 | 52 | 35（−17） |
| `go.sum` 行数 | 46 | 36（−10） |
| Prometheus/protobuf 包 | 41 | **0** |

而**确实要记指标**的程序只比 v3 多付 0.8%，来自接口间接调用和多出的一个包。代价是
转嫁给需要它的服务，而不是凭空消失。

### 你需要改什么

1. **import 路径**，全部：

   ```diff
   -go get github.com/soulteary/httpcache-kit/v3
   +go get github.com/soulteary/httpcache-kit/v4
   ```

2. **如果你用了指标**，导入子包并改两个名字：

   | v3 | v4 |
   |---|---|
   | `httpcache.CacheMetrics`（结构体） | `prometheusmetrics.Metrics` |
   | `httpcache.NewCacheMetrics(reg)` | `prometheusmetrics.New(reg)` |

   ```diff
   +import "github.com/soulteary/httpcache-kit/v4/prometheusmetrics"

   -m := httpcache.NewCacheMetrics(registry)
   +m := prometheusmetrics.New(registry)
    handler.SetMetrics(m)
   ```

   `httpcache.Metrics` 现在指的是缓存上报所用的**接口**，不再是 Prometheus 结构体。
   `SetDefaultMetrics`、`GetDefaultMetrics` 和 `Handler.SetMetrics` 名字不变，改为接收它。

   这里刻意没有保留兼容别名：别名必须 import `prometheus/client_golang`，一 import
   就又把它链回来了，拆分的收益也就没了。

3. **删掉对 `GetDefaultMetrics` 的判空。** 它过去返回 `*CacheMetrics`，在注册指标前
   是 nil，靠 nil receiver 方法保证安全。接口没有这种待遇 —— 对 nil 接口调方法会
   panic —— 所以"未设置"现在是一个真实对象 `NopMetrics`：

   ```diff
   -if m := httpcache.GetDefaultMetrics(); m != nil {
   -    m.RecordCacheHit("GET")
   -}
   +httpcache.GetDefaultMetrics().RecordCacheHit("GET")
   ```

   `SetDefaultMetrics(nil)` 会装上 `NopMetrics`，而不是埋一个 nil。

缓存行为、handler、后端、缓存键、失效逻辑，以及记录的指标名/标签/分桶全部不变 ——
Prometheus 构造代码是原样搬过去的。

### 为什么日志没有一起拆

`logger-kit` 只占剩下 230 个包里的 5 个，而 handler 有些路径除了日志没有别的方式
上报。日志不像指标导出那样是可选的，拆它要付出一个接口加一次 import，换来的却几乎
没有。

## 升级说明（v3.0.0）

缓存自身的 API 没有任何变化。变的是模块路径 —— 本模块的，以及它两个依赖的 ——
因为 `logger-kit` 和 `metrics-kit` 升到了 `/v3`，而缓存 API 会把它们的类型交给你。

1. **改模块路径。** 每个文件里的每处 import：

   ```bash
   go get github.com/soulteary/httpcache-kit/v3
   go mod edit -droprequire github.com/soulteary/httpcache-kit/v2
   ```

   ```diff
   -httpcache "github.com/soulteary/httpcache-kit/v2"
   +httpcache "github.com/soulteary/httpcache-kit/v3"
   ```

   `go get -u` 不会帮你做这件事；v2 停留在 `v2.5.0`。

2. **如果你的代码写到了它们的类型，`logger-kit` 与 `metrics-kit` 也要一起改。**
   `SetLogger`、`HandlerOptions.Logger` 和 `NewCacheMetrics` 接收
   `*logger.Logger` 与 `*metrics.Registry`，而 v2 的类型无法满足 v3 的参数 ——
   模块路径是类型标识的一部分。这是唯一可能编译不过的地方：

   ```diff
   -logger "github.com/soulteary/logger-kit/v2"
   -metrics "github.com/soulteary/metrics-kit/v2"
   +logger "github.com/soulteary/logger-kit/v3"
   +metrics "github.com/soulteary/metrics-kit/v3"
   ```

   本缓存用到的每个名字 —— `logger.Default`、`logger.NewDefault`、
   `logger.Middleware`、`logger.MiddlewareConfig`、`metrics.NewRegistry`、
   `metrics.Registry`、`metrics.HTTPDurationBuckets` —— 签名都没变。如果你用过这
   两个 kit 里的 `FiberHandler`、`NewFiberMiddleware` 或 `SkipFuncFiber` 字段，
   它们已移入各自的 `fiberadapter` 子包，详见那两个 kit 自己的 v3 升级说明。

3. **没有别的了。** 本包没有新增、删除或修改任何名字。import 能编译过，就升级完了。

### 这次升级换来了什么

这两个 kit 把 Fiber 支持移进了 `fiberadapter` 子包，根包不再链接任何 Web 框架 ——
而本缓存从来就没用过 Fiber。它之前一直背着这个框架，只是因为 `logger-kit/v2` 和
`metrics-kit/v2` 把它带了进来：

| | v2.5.0 | v3.0.0 |
| --- | --- | --- |
| 链接的 Fiber/fasthttp/compress/msgp 包数 | 39 | **0** |
| 库链接的包总数 | 340 | 280 |
| build list 中的模块数 | 61 | 51 |
| `go.mod` 里的 `// indirect` 行数 | 24 | 12 |

减掉的十二项依赖是 `gofiber/fiber/v3`、`gofiber/schema`、`gofiber/utils/v2`、
`klauspost/compress`、`molecule-man/go-brrr`、`philhofer/fwd`、`tinylib/msgp`、
`valyala/bytebufferpool`、`valyala/fasthttp`、`golang.org/x/crypto`、
`golang.org/x/net` 和 `golang.org/x/text`。Fiber 仍会出现在 `go list -m all`
里，因为 `logger-kit/v3` 和 `metrics-kit/v3` 为自己的 `fiberadapter` 子包 require
了它 —— 但它的任何包都不会被编译进使用本缓存的二进制。

如果你本来就把这个缓存跑在 Fiber 后面，什么也没丢：你的 Fiber 是从自己的 import
来的，不是从本模块来的。

### `vfs-kit` v1.4.0 → v1.4.2

修复了内存文件系统的数据竞争 —— 也就是 `NewMemoryCache` 和
`NewMemoryCacheWithConfig` 运行其上的那个实现。原来整个文件系统共用一把互斥锁，
现在改为按目录加锁；`File.FileMode` 和压缩读取路径也补上了原本缺失的读锁。本缓存
用到的 API 没有变化；v1.4.2 还新导出了一个 `ErrRemoveRoot`，而本缓存不可能产生它
—— 它只会删除单个条目文件，从不删除文件系统根。

## 升级说明（v2.5.0）

没有新增、移除或改变任何 API。磁盘后端写入条目的方式变了，缓存目录下多了一个新目录。

- **磁盘后端改为通过 rename 发布条目。** 此前用 `O_CREATE|O_TRUNC` 打开目标，新内容从来不是作为一个整体可见的：首次存储时空文件会在任何字节写入之前出现；重新存储时一个完全可读的条目会被清空，持续整个拷贝过程。在 8 个并发读者下替换一个 128 KiB 条目，会产生 2 次 miss 和 258 次「既非旧也非新的不完整 body」；另一个探针在 200 次重新存储期间抓到 header 文件为空 **4543 次**。现在共享同一缓存实例的读者看到的要么是完整的旧条目，要么是完整的新条目 —— 这一保证的两处边界（两次 rename 之间崩溃、同目录上的两个实例）见「磁盘布局」章节。
- **body 与它的 header 一并变更。** `Retrieve` 先打开 body、后读 header，发布若落在这两步之间，就会把一个版本的 payload 配上另一个版本的状态与 header —— 一个描述着「已经不在那里的 body」的 `Content-Length` 或 `Content-Encoding`。压力下 600 次读取有 148 次错配。现在两个文件在同一把锁下 rename，而 `Retrieve` 持该锁跨越两次查找。
- **缓存目录下新增 `staging/v1`。** 条目先写到这里，再 rename 到位。它位于所有被扫描目录之外，因此其中的内容永远不会被误认为条目；启动时会清扫一次 —— 写入途中被杀死的进程会留下文件，而没有别的机制会回收它。**如果你对缓存目录做容量统计、备份或 rsync，请把它一并包含**；缓存空闲时它是空的。
- **存储开销上升，主要体现在小条目上。** 对比 v2.4.0 的每次 store 实测：4 KiB 由 193µs 变为 967µs，2 MiB 由 1.66ms 变为 2.03ms。大条目接近免费；小条目的开销来自针对极短写入的额外系统调用。数字取自容器文件系统，别处会有差异。
- **条目文件不做 fsync。** 让发布具备原子性的是 rename；崩溃后的持久性并非缓存所需，以这种方式丢失的条目不过是一次 miss。`stale-markers.json` 仍然 fsync，因为丢失失效状态会把已被取代的内容重新发布出去。
- **内存后端与其他 VFS 后端不受影响。** `VFS` 接口没有 rename，它们保持就地写。v2.4.0 加入的 header 完整性判定继续把那个窗口转成 miss。

## 升级说明（v2.2.0）

本次发布改变了"哪些条目会被发出"以及"键怎么算"。新增一个构造函数和一个选项，没有删除
任何东西。

- **失效现在真的会发生。** `invalidateResource()` 的整个函数体只是一行调试日志，于是
  一个被 `POST`、`PUT` 或 `DELETE` 过的资源，仍然会从缓存里被发出，直到它自己的新鲜期
  到期。现在它会使请求 URI，以及响应 `Location` 和 `Content-Location` 指向的同源 URI
  失效，`GET` 和 `HEAD` 两个键都处理。**非安全方法之后请预期更多回源流量**——这正是
  被修掉的那个缺陷。
- **请求的 `Content-Location` 不再选择缓存键。** 它此前会，而回源请求用的仍是原始
  URL，于是客户端可以把自己的响应塞到别的 URL 的键下（共享缓存投毒），或读到别的 URL
  的条目。如果你此前是刻意利用它来给条目做别名，没有替代方案——那不是一个特性。
- **带 `Vary` 的键，`Key.String()` 变了。** 它此前用 `":"` 和 `"::"` 拼接原始 URL 和
  原始 header 值，于是精心构造的 URL 可以和另一个带 `Vary` 值的 URL 撞键。现在
  `Vary` 部分采用加引号、以控制字节分隔的编码。**已有的带 `Vary` 的缓存条目会未命中
  一次**并重新取回；不带 `Vary` 的键不变。
- **反向代理请用 `NewSharedHandler`。** `Shared` 默认为 `false`，这对私有缓存是对的、
  对共享缓存是错的——而字段会被忘记，构造函数不会。如果你是手动设置
  `handler.Shared = true`，不会有任何问题；新代码请用构造函数。
- **磁盘后端不再每次 `Vary` 查找泄漏一个文件句柄。** 主 `Resource` 此前被直接覆盖而
  没有关闭。
- **`CacheControl.String()` 不再输出空项。** 它此前用 `make([]string, len(cc))` 分配
  键切片然后再 append，于是输出开头有 `len(cc)` 个空字段。
- **新增 `HandlerOptions.StaleMarkerTTL`。** 当缓存自身不跟踪失效状态时，请把它设成
  不短于缓存的保留时长；否则标记过期会把变更前的条目重新当作新鲜数据发出去。

## 测试

```bash
go test ./...

# 带覆盖率
go test ./... -coverprofile=coverage.out -covermode=atomic
go tool cover -func=coverage.out
```

## 参考

- [RFC 7234](http://httpwg.github.io/specs/rfc7234.html) —— HTTP/1.1 缓存
- [lox/httpcache](https://github.com/lox/httpcache) —— 原始库（MIT）

## 许可证

Apache License 2.0 —— 详见 [LICENSE](LICENSE)。部分代码源自
[lox/httpcache](https://github.com/lox/httpcache)，采用 MIT 许可。
