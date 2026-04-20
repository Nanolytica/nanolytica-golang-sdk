# Nanolytica Go SDK

Go client for [Nanolytica Cloud](https://cloud.nanolytica.org) analytics. Tracks pageviews and custom events from Go servers, CLIs, TV apps, and IoT devices. Zero external dependencies.

## Requirements

- Go 1.22 or later
- A Nanolytica account with at least one site created as **Native App**

## Dashboard setup

1. Sign in at [cloud.nanolytica.org](https://cloud.nanolytica.org) and click **Add site**.
2. Choose **Native App** as the site type (enables app-version display instead of browser names).
3. Copy the site UUID from the Setup tab — it looks like `3f4a1b2c-...`.

## Install

```bash
go get github.com/eringen/nanolytica-cloud/SDK/go
```

Import alias keeps call sites short:

```go
import nanolytica "github.com/eringen/nanolytica-cloud/SDK/go"
```

## Initialization

```go
client, err := nanolytica.New("3f4a1b2c-...", &nanolytica.Options{
    Endpoint:   "https://cloud.nanolytica.org", // default; override for self-hosted
    UserAgent:  "MyApp/1.2.3 (Server; linux)",   // see User-Agent section
    BufferSize: 100,                              // in-memory queue depth
    HTTPClient: nil,                              // nil = 10 s timeout client
})
if err != nil {
    log.Fatal(err) // only fails if siteID is empty
}
defer client.Close()
```

### Options reference

| Field | Type | Default | Description |
|---|---|---|---|
| `Endpoint` | `string` | `https://cloud.nanolytica.org` | Base URL of your Nanolytica instance |
| `UserAgent` | `string` | auto-generated | Sent as `User-Agent` header and stored per event |
| `BufferSize` | `int` | `100` | Max in-memory queue depth; oldest dropped when full |
| `HTTPClient` | `*http.Client` | 10 s timeout | Supply your own to control timeouts, proxies, TLS |

## Recording pageviews

### Basic

```go
ctx := context.Background()

// Minimal — path only
client.Pageview(ctx, "/home", nil)

// With referrer and screen size
client.Pageview(ctx, "/product/42", &nanolytica.PageviewOptions{
    Referrer:   "https://google.com",
    ScreenSize: "1920x1080",
})
```

### Server-side: one pageview per HTTP request

```go
http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
    client.Pageview(r.Context(), r.URL.Path, &nanolytica.PageviewOptions{
        Referrer: r.Referer(),
    })
    // ... serve response
})
```

### PageviewOptions reference

| Field | Description |
|---|---|
| `Referrer` | Previous URL (≤ 2048 chars) |
| `ScreenSize` | `WIDTHxHEIGHT` string, e.g. `"1920x1080"` |
| `UTMSource` | Value of `utm_source` query param |
| `UTMMedium` | Value of `utm_medium` query param |
| `UTMCampaign` | Value of `utm_campaign` query param |
| `UTMContent` | Value of `utm_content` query param |
| `UTMTerm` | Value of `utm_term` query param |

## Custom events

```go
// Simple event — no props, no revenue
client.Track(ctx, "app_open", nil, nil)

// Event with props
client.Track(ctx, "signup", map[string]string{
    "plan":   "pro",
    "source": "pricing-page",
}, nil)

// Event with revenue (summed in the Goals view)
client.Track(ctx, "purchase", map[string]string{
    "plan": "pro",
}, nanolytica.Value(49.99))
```

`nanolytica.Value(v)` is a helper that returns `*float64` so you can pass a literal inline.

## UTM campaign tracking

Pass UTM parameters you extracted from a landing URL or install referrer:

```go
u, _ := url.Parse(deepLink)
q := u.Query()

client.Pageview(ctx, "/home", &nanolytica.PageviewOptions{
    UTMSource:   q.Get("utm_source"),
    UTMMedium:   q.Get("utm_medium"),
    UTMCampaign: q.Get("utm_campaign"),
    UTMContent:  q.Get("utm_content"),
    UTMTerm:     q.Get("utm_term"),
})
```

UTM data appears in the **Sources** tab of your dashboard.

## Flushing and shutdown

`Close()` drains the queue before returning. Use `Flush(ctx)` when you need to wait mid-program:

```go
// Wait up to 5 s for all queued events to send
fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()
if err := client.Flush(fctx); err != nil {
    log.Printf("flush incomplete: %v", err)
}
```

Always call `Close()` (or use `defer client.Close()`) before process exit — unflushed events are lost.

## Privacy and opt-out

The SDK has no opt-out mechanism built in — simply stop calling `Pageview`/`Track` when the user declines analytics consent. A common pattern:

```go
var analyticsEnabled atomic.Bool

func recordPageview(path string) {
    if !analyticsEnabled.Load() {
        return
    }
    client.Pageview(context.Background(), path, nil)
}
```

## Self-hosted instance

```go
client, err := nanolytica.New(siteID, &nanolytica.Options{
    Endpoint: "https://analytics.your-domain.com",
})
```

## User-Agent format

The server parses the `User-Agent` to populate the **App Versions** column (for Native App sites). Use the canonical form:

```
AppName/X.Y.Z (Platform N; Device)
```

Examples:
- `MyServer/2.1.0 (Go go1.22; linux amd64)` — the auto-generated default
- `MyTVApp/3.0.1 (tvOS 17; AppleTV6,2)`
- `IoTSensor/1.0.0 (RTOS 2.4; ESP32)`

The server takes everything before the first space, replaces `/` with a space, and stores it as the app version. `MyServer/2.1.0` → "MyServer 2.1.0" in the dashboard.

## Full API reference

### `New(siteID string, opts *Options) (*Client, error)`

Creates a client and starts the background worker. Returns `ErrInvalidSiteID` if `siteID` is empty.

### `(*Client) Pageview(ctx context.Context, path string, opts *PageviewOptions) error`

Enqueues a pageview. Returns an error only for local validation failures (path too long). The send is non-blocking.

### `(*Client) Track(ctx context.Context, name string, props map[string]string, value *float64) error`

Enqueues a custom event. Returns one of:

| Error | Condition |
|---|---|
| `ErrInvalidEventName` | Name empty, >64 chars, or not matching `^[a-zA-Z0-9_-]+$` |
| `ErrReservedPrefix` | Name starts with `nanolytica_` |
| `ErrTooManyProps` | More than 10 props |
| `ErrPropKey` | Prop key empty, >64 chars, or bad chars |
| `ErrPropValue` | Prop value >256 chars |
| `ErrClosed` | Client has been closed |

### `(*Client) Flush(ctx context.Context) error`

Blocks until the queue (and any in-flight request) is empty, or `ctx` is cancelled. Returns `ctx.Err()` on cancellation.

### `(*Client) Close() error`

Signals the worker to stop after draining. Safe to call multiple times.

### `Value(v float64) *float64`

Helper that converts a float literal to a pointer for `Track`.

## Validation rules

All validation runs locally before any network call:

| Field | Rule |
|---|---|
| `site_id` | Non-empty string (not UUID-validated client-side) |
| `path` | ≤ 2048 chars |
| `referrer` | ≤ 2048 chars |
| `screen_size` | Format `WIDTHxHEIGHT` |
| `event_name` | 1–64 chars, `^[a-zA-Z0-9_-]+$`, not starting with `nanolytica_` |
| `props` keys | 1–64 chars, `^[a-zA-Z0-9_-]+$` |
| `props` values | ≤ 256 chars |
| `props` count | ≤ 10 pairs |

## Transport behavior

- Events are queued in memory and sent from a single background goroutine.
- Server 5xx or network errors retry up to 3 times with 1 s / 2 s / 4 s backoff. 4xx errors (validation) are dropped immediately — never retried.
- When the queue reaches `BufferSize`, the oldest pending event is dropped to make room.
- All methods are safe to call concurrently from multiple goroutines.

## Troubleshooting

**Events not appearing in the dashboard**
- Verify the site UUID is correct and the site type is **Native App**.
- Check that `Close()` or `Flush()` is called before the process exits — unflushed events in the queue are lost on a hard kill.
- Inspect server logs: a 400 response means a validation error; the response body has the reason.

**App Versions shows "Other" instead of my app name**
- The UA must be in the form `AppName/X.Y.Z (...)`. Anything before the first space, with `/` replaced by a space, becomes the version string.
- Confirm the site is created as **Native App** in the dashboard — web sites don't parse the app-version slot.

**Rate limit (429)**
- Default limit is 1000 events/min per site. Reduce call frequency or set `NANOLYTICA_MAX_SITE_EVENTS_PER_MIN` on the server.

**`New` returns an error**
- Only happens when `siteID` is empty. All other options are clamped to safe defaults.

**Multiple services sharing one site**
- Each `New` call creates an independent client with its own queue and goroutine. They share the same `site_id` on the server — combine-site filtering in the dashboard aggregates them.

## Testing

```bash
go test ./...
```

## License

MIT
