// Package crawlerips checks a crawler request's address against the IP ranges
// the crawler's operator publishes. Scrapers often borrow a well-known
// crawler's user agent, so a request that names GPTBot from outside OpenAI's
// ranges isn't GPTBot.
package crawlerips

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/pug-sh/pug/internal/httpx"
	"github.com/pug-sh/pug/internal/slogx"
)

// Verdict is what a crawler request's $crawler_verified says.
type Verdict string

const (
	// Verified: the address is inside the operator's published ranges.
	Verified Verdict = "verified"
	// Failed: the operator publishes ranges, they are loaded, and the address
	// is a public one outside them, likely an impostor.
	Failed Verdict = "failed"
	// Unchecked: the operator publishes none, they haven't loaded, the check is
	// off, or the address isn't public, so the collector saw a proxy or load
	// balancer in front of the site rather than the crawler.
	Unchecked Verdict = "unchecked"
)

const (
	refreshInterval = 6 * time.Hour
	retryInterval   = 5 * time.Minute
	fetchTimeout    = 10 * time.Second
	maxFileBytes    = 5 << 20
)

// sharedSpace is carrier-grade NAT (RFC 6598): never a crawler's own address.
var sharedSpace = netip.MustParsePrefix("100.64.0.0/10")

var fetchFailedCounter metric.Int64Counter

func init() {
	meter := otel.Meter("github.com/pug-sh/pug/internal/crawlerips")
	fetchFailedCounter, _ = meter.Int64Counter(
		"crawlers.ranges_fetch_failed_total",
		metric.WithDescription("A crawler IP-range file could not be fetched or read; the last good copy stays in use, and until a first copy loads its agents' requests read unchecked. Every file failing means the server has no internet access: set PUG_CRAWLER_RANGES_ENABLED=false."),
	)
}

// Checker keeps the last good copy of every range file.
type Checker struct {
	enabled bool
	urls    []string
	client  *http.Client
	loaded  atomic.Bool

	mu     sync.RWMutex
	ranges map[string][]netip.Prefix
}

// New returns a Checker for the files at urls and starts loading them, then
// refreshes them every 6 hours until ctx ends. Disabled, it fetches nothing and
// every Check is Unchecked.
func New(ctx context.Context, enabled bool, urls []string) *Checker {
	c := newChecker(enabled, urls, httpx.NewClient(fetchTimeout))
	c.start(ctx)
	return c
}

func newChecker(enabled bool, urls []string, client *http.Client) *Checker {
	return &Checker{
		enabled: enabled,
		urls:    urls,
		client:  client,
		ranges:  make(map[string][]netip.Prefix, len(urls)),
	}
}

func (c *Checker) start(ctx context.Context) {
	if !c.enabled || len(c.urls) == 0 {
		c.loaded.Store(true)
		return
	}
	go c.refresh(ctx)
}

// Loaded reports whether the first load has finished, whether or not it
// fetched every file.
func (c *Checker) Loaded() bool {
	return c.loaded.Load()
}

// Check returns the verdict for ip against the files at urls, which together
// hold one agent's ranges.
func (c *Checker) Check(urls []string, ip string) Verdict {
	if !c.enabled || len(urls) == 0 {
		return Unchecked
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return Unchecked
	}
	// Node reports an IPv4 client on a dual-stack socket as ::ffff:a.b.c.d.
	addr = addr.Unmap()
	if !public(addr) {
		return Unchecked
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	loaded := true
	for _, u := range urls {
		prefixes, ok := c.ranges[u]
		if !ok {
			loaded = false
			continue
		}
		if slices.ContainsFunc(prefixes, func(p netip.Prefix) bool { return p.Contains(addr) }) {
			return Verified
		}
	}
	if !loaded {
		return Unchecked
	}
	return Failed
}

func public(addr netip.Addr) bool {
	return addr.IsGlobalUnicast() && !addr.IsPrivate() && !sharedSpace.Contains(addr)
}

// refresh retries sooner, backing off, until every file has loaded once.
func (c *Checker) refresh(ctx context.Context) {
	complete := c.loadAll(ctx)
	c.loaded.Store(true)
	retry := retryInterval
	for {
		wait := refreshInterval
		if !complete {
			wait, retry = retry, min(retry*2, refreshInterval)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		complete = c.loadAll(ctx)
	}
}

// loadAll fetches every file at once and reports whether each has a copy. A
// file that fails keeps its last good copy.
func (c *Checker) loadAll(ctx context.Context) bool {
	var wg sync.WaitGroup
	var loaded atomic.Int64
	for _, u := range c.urls {
		wg.Go(func() {
			prefixes, err := c.fetch(ctx, u)
			if err != nil {
				if ctx.Err() == nil {
					slog.WarnContext(ctx, "crawler IP ranges not loaded, keeping the last good copy",
						slogx.Error(err), slog.String("url", u))
					fetchFailedCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("url", u)))
				}
				return
			}
			c.mu.Lock()
			c.ranges[u] = prefixes
			c.mu.Unlock()
			loaded.Add(1)
		})
	}
	wg.Wait()
	if n := loaded.Load(); n == 0 && ctx.Err() == nil {
		slog.WarnContext(ctx, "no crawler IP ranges loaded; without internet access, set PUG_CRAWLER_RANGES_ENABLED=false",
			slog.Int("files", len(c.urls)))
	} else {
		slog.InfoContext(ctx, "crawler IP ranges refreshed", slog.Int64("loaded", n), slog.Int("files", len(c.urls)))
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.ranges) == len(c.urls)
}

func (c *Checker) fetch(ctx context.Context, url string) ([]netip.Prefix, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(http.MaxBytesReader(nil, resp.Body, maxFileBytes))
	if err != nil {
		return nil, err
	}
	return parse(body)
}

// parse takes every string in a prefixes entry: Amazon's pages wrap the JSON in
// <pre> and use bare addresses or ip_prefix. An empty file would fail every request.
func parse(body []byte) ([]netip.Prefix, error) {
	body = bytes.TrimSpace(body)
	if !bytes.HasPrefix(body, []byte("{")) {
		var ok bool
		if body, ok = preBlock(body); !ok {
			return nil, errors.New("neither JSON nor a page with a <pre> block")
		}
	}
	var doc struct {
		Prefixes []map[string]any `json:"prefixes"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	var out []netip.Prefix
	for _, entry := range doc.Prefixes {
		for _, v := range entry {
			s, ok := v.(string)
			if !ok {
				continue
			}
			if p, err := netip.ParsePrefix(s); err == nil {
				out = append(out, p.Masked())
			} else if a, err := netip.ParseAddr(s); err == nil {
				out = append(out, netip.PrefixFrom(a, a.BitLen()))
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no prefixes")
	}
	return out, nil
}

// preBlock returns the JSON object inside page's first <pre> block.
func preBlock(page []byte) ([]byte, bool) {
	start := bytes.Index(page, []byte("<pre"))
	if start < 0 {
		return nil, false
	}
	end := bytes.Index(page[start:], []byte("</pre>"))
	if end < 0 {
		return nil, false
	}
	block := page[start : start+end]
	first, last := bytes.IndexByte(block, '{'), bytes.LastIndexByte(block, '}')
	if first < 0 || last < first {
		return nil, false
	}
	return []byte(html.UnescapeString(string(block[first : last+1]))), true
}
