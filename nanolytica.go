// Package nanolytica is the Go SDK for Nanolytica Cloud analytics.
//
// Send pageviews and custom events to a Nanolytica instance:
//
//	client, err := nanolytica.New("your-site-uuid", nil)
//	if err != nil { return }
//	defer client.Close()
//	client.Pageview(ctx, "/home", nil)
//	client.Track(ctx, "signup", map[string]string{"plan": "pro"}, nanolytica.Value(49.99))
package nanolytica

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	Version            = "0.1.0"
	DefaultEndpoint    = "https://cloud.nanolytica.org"
	DefaultBufferSize  = 100
	DefaultHTTPTimeout = 10 * time.Second

	maxEventNameLen = 64
	maxPropCount    = 10
	maxPropKeyLen   = 64
	maxPropValueLen = 256
	maxPathLen      = 2048
	maxReferrerLen  = 2048
	maxUserAgentLen = 512
	reservedPrefix  = "nanolytica_"
)

var (
	nameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

	ErrInvalidSiteID    = errors.New("nanolytica: site_id must be a non-empty UUID string")
	ErrInvalidEventName = errors.New("nanolytica: event name must match ^[a-zA-Z0-9_-]+$ and be 1-64 chars")
	ErrReservedPrefix   = errors.New("nanolytica: event names starting with 'nanolytica_' are reserved")
	ErrTooManyProps     = errors.New("nanolytica: at most 10 props allowed")
	ErrPropKey          = errors.New("nanolytica: prop key must match ^[a-zA-Z0-9_-]+$ and be 1-64 chars")
	ErrPropValue        = errors.New("nanolytica: prop value exceeds 256 chars")
	ErrClosed           = errors.New("nanolytica: client is closed")
)

// Options configures a Client.
type Options struct {
	// Endpoint is the Nanolytica instance base URL. Defaults to DefaultEndpoint.
	Endpoint string
	// UserAgent sent with every request. If empty, a sensible default is used.
	UserAgent string
	// BufferSize bounds the in-memory queue. Defaults to 100. When the queue is
	// full, the oldest pending event is dropped to make room.
	BufferSize int
	// HTTPClient lets callers supply a custom http.Client. If nil, a client with
	// DefaultHTTPTimeout is used.
	HTTPClient *http.Client
}

// Client sends analytics events asynchronously to a Nanolytica instance.
type Client struct {
	siteID   string
	endpoint string
	ua       string
	http     *http.Client

	mu      sync.Mutex
	queue   []request
	maxBuf  int
	cond    *sync.Cond
	closed  bool
	sending bool
	wg      sync.WaitGroup
}

type request struct {
	body []byte
}

// Value returns a pointer to v so revenue values can be passed inline.
func Value(v float64) *float64 { return &v }

// PageviewOptions carries optional fields for a Pageview call.
type PageviewOptions struct {
	Referrer    string
	ScreenSize  string
	UTMSource   string
	UTMMedium   string
	UTMCampaign string
	UTMContent  string
	UTMTerm     string
}

// New creates a Client. siteID must be your site UUID from the dashboard.
// opts may be nil to accept defaults.
func New(siteID string, opts *Options) (*Client, error) {
	if !regexp.MustCompile(`^[0-9a-fA-F]{8}(-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}$`).MatchString(siteID) {
		return nil, ErrInvalidSiteID
	}
	o := Options{}
	if opts != nil {
		o = *opts
	}
	if o.Endpoint == "" {
		o.Endpoint = DefaultEndpoint
	}
	u, err := url.Parse(o.Endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, errors.New("nanolytica: endpoint must be an HTTP(S) base URL")
	}
	o.Endpoint = strings.TrimRight(o.Endpoint, "/")
	if len(o.UserAgent) > maxUserAgentLen {
		return nil, errors.New("nanolytica: user agent exceeds 512 bytes")
	}
	if o.BufferSize <= 0 {
		o.BufferSize = DefaultBufferSize
	}
	if o.UserAgent == "" {
		o.UserAgent = fmt.Sprintf("NanolyticaGoSDK/%s (Go %s; %s %s)", Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: DefaultHTTPTimeout}
	}

	c := &Client{
		siteID:   siteID,
		endpoint: o.Endpoint,
		ua:       o.UserAgent,
		http:     o.HTTPClient,
		maxBuf:   o.BufferSize,
	}
	c.cond = sync.NewCond(&c.mu)
	c.wg.Add(1)
	go c.run()
	return c, nil
}

// Pageview records a pageview for the given path.
func (c *Client) Pageview(ctx context.Context, path string, opts *PageviewOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.ContainsAny(path, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f\x7f") {
		return errors.New("nanolytica: invalid path")
	}
	if len(path) > maxPathLen {
		return fmt.Errorf("nanolytica: path exceeds %d chars", maxPathLen)
	}
	payload := map[string]any{
		"site_id":    c.siteID,
		"path":       path,
		"user_agent": c.ua,
	}
	if opts != nil {
		if opts.ScreenSize != "" && (len(opts.ScreenSize) > 20 || !regexp.MustCompile(`^[0-9]{1,5}x[0-9]{1,5}$`).MatchString(opts.ScreenSize)) {
			return errors.New("nanolytica: invalid screen size")
		}
		for _, v := range []string{opts.UTMSource, opts.UTMMedium, opts.UTMCampaign, opts.UTMContent, opts.UTMTerm} {
			if len(v) > 256 {
				return errors.New("nanolytica: campaign value exceeds 256 bytes")
			}
		}
		if opts.Referrer != "" {
			if len(opts.Referrer) > maxReferrerLen {
				return fmt.Errorf("nanolytica: referrer exceeds %d chars", maxReferrerLen)
			}
			payload["referrer"] = opts.Referrer
		}
		if opts.ScreenSize != "" {
			payload["screen_size"] = opts.ScreenSize
		}
		if opts.UTMSource != "" {
			payload["utm_source"] = opts.UTMSource
		}
		if opts.UTMMedium != "" {
			payload["utm_medium"] = opts.UTMMedium
		}
		if opts.UTMCampaign != "" {
			payload["utm_campaign"] = opts.UTMCampaign
		}
		if opts.UTMContent != "" {
			payload["utm_content"] = opts.UTMContent
		}
		if opts.UTMTerm != "" {
			payload["utm_term"] = opts.UTMTerm
		}
	}
	return c.enqueue(payload)
}

// Track fires a custom event. props may be nil. value may be nil for non-revenue events.
func (c *Client) Track(ctx context.Context, name string, props map[string]string, value *float64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateEventName(name); err != nil {
		return err
	}
	if err := validateProps(props); err != nil {
		return err
	}
	payload := map[string]any{
		"site_id":    c.siteID,
		"event_name": name,
		"user_agent": c.ua,
	}
	if len(props) > 0 {
		payload["props"] = props
	}
	if value != nil {
		payload["value"] = *value
	}
	return c.enqueue(payload)
}

// Flush blocks until the queue is drained (including any in-flight request)
// or ctx is cancelled.
func (c *Client) Flush(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	stop := context.AfterFunc(ctx, func() {
		c.mu.Lock()
		c.cond.Broadcast()
		c.mu.Unlock()
	})
	defer stop()
	for len(c.queue) > 0 || c.sending {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.cond.Wait()
	}
	return ctx.Err()
}

// Close stops the worker after flushing the current queue.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.cond.Broadcast()
	c.mu.Unlock()
	c.wg.Wait()
	return nil
}

func (c *Client) enqueue(payload map[string]any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}
	if len(c.queue) >= c.maxBuf {
		// Drop oldest.
		c.queue = c.queue[1:]
	}
	c.queue = append(c.queue, request{body: body})
	c.cond.Broadcast()
	return nil
}

func (c *Client) run() {
	defer c.wg.Done()
	for {
		c.mu.Lock()
		for len(c.queue) == 0 && !c.closed {
			c.cond.Wait()
		}
		if len(c.queue) == 0 && c.closed {
			c.mu.Unlock()
			return
		}
		next := c.queue[0]
		c.queue = c.queue[1:]
		c.sending = true
		c.mu.Unlock()

		c.send(next.body)

		c.mu.Lock()
		c.sending = false
		c.cond.Broadcast()
		c.mu.Unlock()
	}
}

func (c *Client) send(body []byte) {
	url := c.endpoint + "/api/collect"
	backoff := []time.Duration{0, 1 * time.Second, 2 * time.Second, 4 * time.Second}
	for _, wait := range backoff {
		if wait > 0 {
			time.Sleep(wait)
		}
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", c.ua)
		resp, err := c.http.Do(req)
		if err != nil {
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusRequestTimeout {
			// Drop permanent validation failures; retry throttling.
			return
		}
	}
}

func validateEventName(name string) error {
	if name == "" || len(name) > maxEventNameLen || !nameRe.MatchString(name) {
		return ErrInvalidEventName
	}
	if hasPrefix(name, reservedPrefix) {
		return ErrReservedPrefix
	}
	return nil
}

func validateProps(props map[string]string) error {
	if len(props) > maxPropCount {
		return ErrTooManyProps
	}
	for k, v := range props {
		if k == "" || len(k) > maxPropKeyLen || !nameRe.MatchString(k) {
			return ErrPropKey
		}
		if len(v) > maxPropValueLen {
			return ErrPropValue
		}
	}
	return nil
}

func hasPrefix(s, p string) bool {
	return len(s) >= len(p) && s[:len(p)] == p
}
