package crawlerips

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// The three shapes the 23 files come in: every operator's JSON, and Amazon's
// page with bare addresses (two of its files) or with ip_prefix (the third).
func TestParse(t *testing.T) {
	tests := []struct {
		name, body string
		want       []string
	}{
		{
			"JSON",
			`{"creationTime": "2026-08-11T13:40:00Z", "prefixes": [{"ipv6Prefix": "2600:1f28:365:8000::/56"}, {"ipv4Prefix": "3.41.188.32/29"}]}`,
			[]string{"2600:1f28:365:8000::/56", "3.41.188.32/29"},
		},
		{
			"Amazon, bare addresses",
			`<button class="copybtn"><span class="icopy">&#9112;</span>Copy code</button>
<pre><code class="container">{
  "creationTime": "2026-09-08T05:24:10.610952+00:00",
  "prefixes": [
    {
      "ipv4Prefix": "3.81.245.78"
    },
    {
      "ipv4Prefix": "3.81.253.213"
    }
  ]
}</code></pre>`,
			[]string{"3.81.245.78/32", "3.81.253.213/32"},
		},
		{
			"Amazon, ip_prefix",
			`<pre><code class="container">{
  "creationTime": "2026-09-08T05:24:10.610952+00:00",
  "prefixes": [
    {
      "ip_prefix": "3.82.67.224/32"
    }
  ]
}</code></pre>`,
			[]string{"3.82.67.224/32"},
		},
	}
	for _, tt := range tests {
		got, err := parse([]byte(tt.body))
		if err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		if len(got) != len(tt.want) {
			t.Errorf("%s: parsed %v, want %v", tt.name, got, tt.want)
		}
		for _, w := range tt.want {
			if !slices.Contains(got, netip.MustParsePrefix(w)) {
				t.Errorf("%s: missing %s in %v", tt.name, w, got)
			}
		}
	}
}

func TestParseRefuses(t *testing.T) {
	for name, body := range map[string]string{
		"no prefixes":     `{"creationTime": "2026-11-16T00:00:00Z", "prefixes": []}`,
		"no addresses":    `{"prefixes": [{"ipv4Prefix": "not an address"}, {"note": 4}]}`,
		"not JSON":        `{"prefixes": [`,
		"page without it": `<html><body>maintenance</body></html>`,
		"empty":           ``,
	} {
		if got, err := parse([]byte(body)); err == nil {
			t.Errorf("%s: parsed %v, want an error", name, got)
		}
	}
}

const (
	openAI   = "https://openai.com/gptbot.json"
	google1  = "https://developers.google.com/static/crawling/ipranges/user-triggered-fetchers.json"
	google2  = "https://developers.google.com/static/crawling/ipranges/user-triggered-fetchers-google.json"
	unloaded = "https://example.com/never-loaded.json"
)

func loadedChecker(t *testing.T) *Checker {
	t.Helper()
	c := newChecker(true, nil, nil)
	c.ranges[openAI] = []netip.Prefix{netip.MustParsePrefix("20.171.207.0/24"), netip.MustParsePrefix("2603:1030::/32")}
	c.ranges[google1] = []netip.Prefix{netip.MustParsePrefix("34.98.136.0/24")}
	return c
}

func TestCheck(t *testing.T) {
	c := loadedChecker(t)
	tests := []struct {
		name string
		urls []string
		ip   string
		want Verdict
	}{
		{"inside", []string{openAI}, "20.171.207.5", Verified},
		{"inside, IPv6", []string{openAI}, "2603:1030:7::1", Verified},
		{"inside, IPv4 in IPv6 form", []string{openAI}, "::ffff:20.171.207.5", Verified},
		{"outside", []string{openAI}, "203.0.114.9", Failed},
		{"outside, IPv6", []string{openAI}, "2a00:1450::1", Failed},
		{"private", []string{openAI}, "10.0.0.5", Unchecked},
		{"private, IPv4 in IPv6 form", []string{openAI}, "::ffff:192.168.1.10", Unchecked},
		{"loopback", []string{openAI}, "127.0.0.1", Unchecked},
		{"link-local", []string{openAI}, "fe80::1", Unchecked},
		{"shared address space", []string{openAI}, "100.64.3.4", Unchecked},
		{"unique local", []string{openAI}, "fd00::1", Unchecked},
		{"no address", []string{openAI}, "", Unchecked},
		{"not an address", []string{openAI}, "crawler", Unchecked},
		{"no published ranges", nil, "20.171.207.5", Unchecked},
		{"not loaded yet", []string{unloaded}, "20.171.207.5", Unchecked},
		{"in the loaded file of two", []string{google1, google2}, "34.98.136.7", Verified},
		{"outside the loaded file of two", []string{google1, google2}, "203.0.114.9", Unchecked},
	}
	for _, tt := range tests {
		if got := c.Check(tt.urls, tt.ip); got != tt.want {
			t.Errorf("%s: Check(%v, %q) = %s, want %s", tt.name, tt.urls, tt.ip, got, tt.want)
		}
	}

	c.ranges[google2] = []netip.Prefix{netip.MustParsePrefix("74.125.0.0/16")}
	if got := c.Check([]string{google1, google2}, "203.0.114.9"); got != Failed {
		t.Errorf("outside both loaded files = %s, want failed", got)
	}
}

func TestCheckDisabled(t *testing.T) {
	c := newChecker(false, []string{openAI}, nil)
	c.start(t.Context())
	if !c.Loaded() {
		t.Error("a disabled checker never reports loaded")
	}
	c.ranges[openAI] = []netip.Prefix{netip.MustParsePrefix("20.171.207.0/24")}
	if got := c.Check([]string{openAI}, "20.171.207.5"); got != Unchecked {
		t.Errorf("disabled Check = %s, want unchecked", got)
	}
}

// rangeServer serves body with status, as the operator's file at /ranges.json.
type rangeServer struct {
	status atomic.Int64
	body   atomic.Value
}

func newRangeServer(t *testing.T, body string) (*rangeServer, *httptest.Server) {
	t.Helper()
	rs := &rangeServer{}
	rs.status.Store(http.StatusOK)
	rs.body.Store(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(rs.status.Load()))
		_, _ = w.Write([]byte(rs.body.Load().(string)))
	}))
	t.Cleanup(srv.Close)
	return rs, srv
}

func TestLoadAllKeepsTheLastGoodCopy(t *testing.T) {
	rs, srv := newRangeServer(t, `{"prefixes": [{"ipv4Prefix": "20.171.207.0/24"}]}`)
	url := srv.URL + "/ranges.json"
	c := newChecker(true, []string{url}, srv.Client())

	c.loadAll(t.Context())
	if got := c.Check([]string{url}, "20.171.207.5"); got != Verified {
		t.Fatalf("after the first load = %s, want verified", got)
	}

	for name, bad := range map[string]func(){
		"server error": func() { rs.status.Store(http.StatusInternalServerError) },
		"empty file":   func() { rs.status.Store(http.StatusOK); rs.body.Store(`{"prefixes": []}`) },
		"over 5 MB": func() {
			rs.status.Store(http.StatusOK)
			rs.body.Store(`{"prefixes": [{"ipv4Prefix": "198.51.100.0/24"}], "pad": "` + strings.Repeat("x", maxFileBytes) + `"}`)
		},
	} {
		bad()
		c.loadAll(t.Context())
		if got := c.Check([]string{url}, "20.171.207.5"); got != Verified {
			t.Errorf("%s: the last good copy was dropped: %s", name, got)
		}
	}
}

// fakeFiles serves every range file from memory, failing the URLs in fail.
type fakeFiles struct {
	mu   sync.Mutex
	hits map[string]int
	fail map[string]bool
}

func (f *fakeFiles) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits[req.URL.String()]++
	if f.fail[req.URL.String()] {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody, Request: req}, nil
	}
	body := `{"prefixes": [{"ipv4Prefix": "20.171.207.0/24"}]}`
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

func (f *fakeFiles) count(url string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[url]
}

func TestRefreshSchedule(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		files := &fakeFiles{hits: map[string]int{}, fail: map[string]bool{google1: true}}
		c := newChecker(true, []string{openAI, google1}, &http.Client{Transport: files})
		const ip = "20.171.207.5"

		if c.Loaded() {
			t.Fatal("loaded before starting")
		}
		c.start(ctx)
		synctest.Wait()
		// A file that fails still finishes the first load, which readiness waits on.
		if !c.Loaded() || files.count(openAI) != 1 {
			t.Fatalf("after the first load: loaded %v, fetched %d times", c.Loaded(), files.count(openAI))
		}
		if got := c.Check([]string{google1}, ip); got != Unchecked {
			t.Fatalf("a file that failed its first load = %s, want unchecked", got)
		}

		files.mu.Lock()
		files.fail[google1] = false
		files.mu.Unlock()
		time.Sleep(retryInterval)
		synctest.Wait()
		if got := c.Check([]string{google1}, ip); got != Verified {
			t.Errorf("a file that failed its first load was not retried: %s", got)
		}

		time.Sleep(refreshInterval)
		synctest.Wait()
		if n := files.count(openAI); n != 3 {
			t.Errorf("fetched %d times, want the first load, the retry and the refresh", n)
		}
	})
}

func TestCheckerStopsWithItsContext(t *testing.T) {
	_, srv := newRangeServer(t, `{"prefixes": [{"ipv4Prefix": "20.171.207.0/24"}]}`)
	ctx, cancel := context.WithCancel(t.Context())
	c := newChecker(true, []string{srv.URL}, srv.Client())
	done := make(chan struct{})
	go func() {
		c.refresh(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh outlived its context")
	}
}
