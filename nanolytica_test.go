package nanolytica

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type captured struct {
	mu   sync.Mutex
	body []map[string]any
}

func (c *captured) append(b map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.body = append(c.body, b)
}

func (c *captured) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.body)
}

func (c *captured) at(i int) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.body[i]
}

func newTestServer(t *testing.T) (*httptest.Server, *captured, *atomic.Int32) {
	t.Helper()
	cap := &captured{}
	status := &atomic.Int32{}
	status.Store(204)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/collect" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		cap.append(m)
		w.WriteHeader(int(status.Load()))
	}))
	return srv, cap, status
}

func mustClient(t *testing.T, endpoint string) *Client {
	t.Helper()
	c, err := New("11111111-1111-4111-8111-111111111111", &Options{Endpoint: endpoint, BufferSize: 10})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestPageview(t *testing.T) {
	srv, cap, _ := newTestServer(t)
	defer srv.Close()
	c := mustClient(t, srv.URL)
	defer c.Close()

	if err := c.Pageview(context.Background(), "/home", &PageviewOptions{Referrer: "https://example.com", ScreenSize: "390x844"}); err != nil {
		t.Fatalf("Pageview: %v", err)
	}
	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if cap.len() != 1 {
		t.Fatalf("want 1 request, got %d", cap.len())
	}
	got := cap.at(0)
	if got["site_id"] != "11111111-1111-4111-8111-111111111111" {
		t.Errorf("site_id=%v", got["site_id"])
	}
	if got["path"] != "/home" {
		t.Errorf("path=%v", got["path"])
	}
	if got["referrer"] != "https://example.com" {
		t.Errorf("referrer=%v", got["referrer"])
	}
}

func TestTrackEvent(t *testing.T) {
	srv, cap, _ := newTestServer(t)
	defer srv.Close()
	c := mustClient(t, srv.URL)
	defer c.Close()

	if err := c.Track(context.Background(), "signup", map[string]string{"plan": "pro"}, Value(49.99)); err != nil {
		t.Fatalf("Track: %v", err)
	}
	_ = c.Flush(context.Background())

	if cap.len() != 1 {
		t.Fatalf("want 1, got %d", cap.len())
	}
	got := cap.at(0)
	if got["event_name"] != "signup" {
		t.Errorf("event_name=%v", got["event_name"])
	}
	if got["value"].(float64) != 49.99 {
		t.Errorf("value=%v", got["value"])
	}
	props := got["props"].(map[string]any)
	if props["plan"] != "pro" {
		t.Errorf("props.plan=%v", props["plan"])
	}
}

func TestValidation(t *testing.T) {
	c := mustClient(t, "http://unused")
	defer c.Close()

	if err := c.Track(context.Background(), "", nil, nil); err != ErrInvalidEventName {
		t.Errorf("empty name: %v", err)
	}
	if err := c.Track(context.Background(), "bad name!", nil, nil); err != ErrInvalidEventName {
		t.Errorf("bad chars: %v", err)
	}
	if err := c.Track(context.Background(), "nanolytica_outbound", nil, nil); err != ErrReservedPrefix {
		t.Errorf("reserved: %v", err)
	}
	too := map[string]string{}
	for i := 0; i < 11; i++ {
		too[string(rune('a'+i))] = "v"
	}
	if err := c.Track(context.Background(), "evt", too, nil); err != ErrTooManyProps {
		t.Errorf("too many props: %v", err)
	}
	if err := c.Track(context.Background(), "evt", map[string]string{"bad key": "v"}, nil); err != ErrPropKey {
		t.Errorf("bad prop key: %v", err)
	}
}

func TestBufferOverflowDropsOldest(t *testing.T) {
	// Server that blocks until we close it, so the worker is stuck on the first
	// request and the queue fills up.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(204)
	}))
	defer srv.Close()

	c, err := New("11111111-1111-4111-8111-111111111111", &Options{Endpoint: srv.URL, BufferSize: 3})
	if err != nil {
		t.Fatal(err)
	}

	// Fire enough events to overflow the buffer.
	for i := 0; i < 10; i++ {
		_ = c.Track(context.Background(), "evt", map[string]string{"i": string(rune('0' + i%10))}, nil)
	}
	close(release)
	_ = c.Close()
	// Can't check exact drop behavior from outside, but this ensures no deadlock
	// and no panic when overflowing.
}

func TestNoRetryOn4xx(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(400)
	}))
	defer srv.Close()

	c := mustClient(t, srv.URL)
	_ = c.Pageview(context.Background(), "/home", nil)
	_ = c.Flush(context.Background())
	_ = c.Close()

	if hits.Load() != 1 {
		t.Errorf("want 1 hit (no retry on 4xx), got %d", hits.Load())
	}
}

func TestRetryOn5xx(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n < 3 {
			w.WriteHeader(502)
			return
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()

	c := mustClient(t, srv.URL)
	_ = c.Pageview(context.Background(), "/home", nil)
	// Allow multiple backoff rounds.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = c.Flush(ctx)
	_ = c.Close()

	if hits.Load() < 3 {
		t.Errorf("want ≥3 hits (retry on 5xx), got %d", hits.Load())
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	c, err := New("11111111-1111-4111-8111-111111111111", &Options{Endpoint: "http://unused"})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if err := c.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if err := c.Pageview(context.Background(), "/x", nil); err != ErrClosed {
		t.Errorf("Pageview after close: %v", err)
	}
}

func TestInvalidSiteID(t *testing.T) {
	if _, err := New("", nil); err != ErrInvalidSiteID {
		t.Errorf("empty siteID: %v", err)
	}
}

func TestTrailingSlashAndThrottle(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/collect" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if hits.Add(1) == 1 {
			w.WriteHeader(429)
		} else {
			w.WriteHeader(204)
		}
	}))
	defer srv.Close()
	c := mustClient(t, srv.URL+"/")
	defer c.Close()
	_ = c.Pageview(context.Background(), "/", nil)
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 2 {
		t.Fatalf("hits = %d", hits.Load())
	}
}

func TestFlushCancellationAndClose(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.WriteHeader(204)
	}))
	defer srv.Close()
	c := mustClient(t, srv.URL)
	_ = c.Pageview(context.Background(), "/", nil)
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if c.Flush(ctx) != context.Canceled {
		t.Fatal("flush ignored cancellation")
	}
	if c.Pageview(ctx, "/", nil) != context.Canceled {
		t.Fatal("enqueue ignored cancellation")
	}
	closing := make(chan struct{})
	go func() { c.Close(); close(closing) }()
	flushed := make(chan struct{})
	go func() { c.Flush(context.Background()); close(flushed) }()
	select {
	case <-flushed:
		t.Fatal("flush returned before delivery")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	<-flushed
	<-closing
}
