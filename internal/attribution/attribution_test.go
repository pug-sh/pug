package attribution

import (
	"reflect"
	"testing"
)

// TestOutputPairsCoversEveryField pins Pairs to Output's shape. Pairs is the
// entire write side of the enrichment — both the ingest handler and the demo
// seeder iterate it and nothing else — so a value added to Output but not to
// Pairs would be derived correctly and then dropped on the floor by every
// caller, with no error and no column ever populated.
func TestOutputPairsCoversEveryField(t *testing.T) {
	var out Output
	rt := reflect.TypeOf(out)

	// Stamp each field with its own name, then require it to surface.
	rv := reflect.ValueOf(&out).Elem()
	for i := range rt.NumField() {
		rv.Field(i).SetString(rt.Field(i).Name)
	}

	pairs := out.Pairs()
	if len(pairs) != rt.NumField() {
		t.Fatalf("Output has %d fields but Pairs returns %d entries", rt.NumField(), len(pairs))
	}
	seen := make(map[string]bool, rt.NumField())
	for _, p := range pairs {
		if p.Key == "" {
			t.Errorf("Pairs entry for value %q has no key", p.Value)
		}
		seen[p.Value] = true
	}
	for i := range rt.NumField() {
		if name := rt.Field(i).Name; !seen[name] {
			t.Errorf("Output.%s never reaches Pairs, so no caller can ever write it", name)
		}
	}
}

func TestDeriveURLDecomposition(t *testing.T) {
	cases := []struct {
		name             string
		in               Input
		pathname, host   string
		utmSource        string
		utmTerm, utmCont string
	}{
		{
			name:     "plain page url",
			in:       Input{URL: "https://Shop.Example.com/products/ball"},
			pathname: "/products/ball",
			host:     "shop.example.com",
		},
		{
			name:     "empty path becomes root",
			in:       Input{URL: "https://example.com"},
			pathname: "/",
			host:     "example.com",
		},
		{
			name:     "port stripped from hostname",
			in:       Input{URL: "http://example.com:8080/x"},
			pathname: "/x",
			host:     "example.com",
		},
		{
			name:     "query and fragment excluded from pathname",
			in:       Input{URL: "https://example.com/search?q=bones#top"},
			pathname: "/search",
			host:     "example.com",
		},
		{
			// Decoded, and identical to the literal-space form below: the
			// two spellings of one page must not become two rollup dim_values.
			name:     "escaped path decoded",
			in:       Input{URL: "https://example.com/a%20b/c"},
			pathname: "/a b/c",
			host:     "example.com",
		},
		{
			name:     "literal space path matches its escaped spelling",
			in:       Input{URL: "https://example.com/a b/c"},
			pathname: "/a b/c",
			host:     "example.com",
		},
		{
			name:     "non-ascii path decoded",
			in:       Input{URL: "https://example.com/caf%C3%A9"},
			pathname: "/café",
			host:     "example.com",
		},
		{
			name:     "literal non-ascii path matches its escaped spelling",
			in:       Input{URL: "https://example.com/café"},
			pathname: "/café",
			host:     "example.com",
		},
		{
			name: "garbage url derives nothing",
			in:   Input{URL: "not a url"},
		},
		{
			name: "non-http scheme derives nothing",
			in:   Input{URL: "ftp://example.com/x"},
		},
		{
			name: "schemeless url derives nothing",
			in:   Input{URL: "example.com/x"},
		},
		{
			name: "no url derives nothing",
			in:   Input{},
		},
		{
			name:     "client pathname wins over derived",
			in:       Input{URL: "https://example.com/real/path", Pathname: "/logical/route"},
			pathname: "/logical/route",
			host:     "example.com",
		},
		{
			name:     "client hostname wins over derived",
			in:       Input{URL: "https://example.com/x", Hostname: "app.example.com"},
			pathname: "/x",
			host:     "app.example.com",
		},
		{
			name:      "utm completed from url query",
			in:        Input{URL: "https://example.com/?utm_source=google&utm_term=dog%20food&utm_content=ad-1"},
			pathname:  "/",
			host:      "example.com",
			utmSource: "google",
			utmTerm:   "dog food",
			utmCont:   "ad-1",
		},
		{
			name:      "client utm wins over url query",
			in:        Input{URL: "https://example.com/?utm_source=bing", UTMSource: "google"},
			pathname:  "/",
			host:      "example.com",
			utmSource: "google",
		},
		{
			name:      "utm not extracted from invalid url",
			in:        Input{URL: "example.com/?utm_source=google"},
			utmSource: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := Derive(c.in)
			if out.Pathname != c.pathname {
				t.Errorf("Pathname = %q, want %q", out.Pathname, c.pathname)
			}
			if out.Hostname != c.host {
				t.Errorf("Hostname = %q, want %q", out.Hostname, c.host)
			}
			if out.UTMSource != c.utmSource {
				t.Errorf("UTMSource = %q, want %q", out.UTMSource, c.utmSource)
			}
			if out.UTMTerm != c.utmTerm {
				t.Errorf("UTMTerm = %q, want %q", out.UTMTerm, c.utmTerm)
			}
			if out.UTMContent != c.utmCont {
				t.Errorf("UTMContent = %q, want %q", out.UTMContent, c.utmCont)
			}
		})
	}
}

func TestDeriveReferrerDomain(t *testing.T) {
	cases := []struct {
		name string
		in   Input
		want string
	}{
		{"external referrer", Input{URL: "https://example.com/x", Referrer: "https://news.ycombinator.com/item?id=1"}, "news.ycombinator.com"},
		{"lowercased", Input{URL: "https://example.com/x", Referrer: "https://Reddit.COM/r/pugs"}, "reddit.com"},
		{"one leading www stripped", Input{URL: "https://example.com/x", Referrer: "https://www.google.com"}, "google.com"},
		{"second www kept", Input{URL: "https://example.com/x", Referrer: "https://www.www.odd.com"}, "www.odd.com"},
		{"self-referral blanked", Input{URL: "https://example.com/x", Referrer: "https://example.com/prev"}, ""},
		{"self-referral blanked across www", Input{URL: "https://www.example.com/x", Referrer: "https://example.com/prev"}, ""},
		{"self-referral blanked www on referrer", Input{URL: "https://example.com/x", Referrer: "https://www.example.com/prev"}, ""},
		// Pinned v1 behavior: subdomains are NOT collapsed (no publicsuffix) —
		// app.example.com referred from www.example.com stays a referral.
		{"subdomain not collapsed", Input{URL: "https://app.example.com/x", Referrer: "https://www.example.com/"}, "example.com"},
		{"non-http scheme with host accepted", Input{URL: "https://example.com/x", Referrer: "android-app://com.google.android.gm"}, "com.google.android.gm"},
		{"protocol-relative referrer accepted", Input{URL: "https://example.com/x", Referrer: "//other.com/x"}, "other.com"},
		{"empty referrer", Input{URL: "https://example.com/x"}, ""},
		{"hostless referrer ignored", Input{URL: "https://example.com/x", Referrer: "not a url"}, ""},
		// No page URL and no client hostname → no self-comparison possible; the
		// referrer domain still derives (always server-derived).
		{"referrer without page url", Input{Referrer: "https://www.google.com"}, "google.com"},
		{"self blank against client hostname", Input{Hostname: "example.com", Referrer: "https://www.example.com/p"}, ""},
		// A client-sent $hostname must NOT steer the server-only
		// $referrerDomain: when a $url is present its host decides self-ness,
		// so neither of these can be influenced from the client.
		{"client hostname cannot defeat self-blanking", Input{URL: "https://real.example.com/p", Hostname: "app", Referrer: "https://real.example.com/x"}, ""},
		{"client hostname cannot suppress a real referrer", Input{URL: "https://real.example.com/p", Hostname: "google.com", Referrer: "https://google.com/s"}, "google.com"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if out := Derive(c.in); out.ReferrerDomain != c.want {
				t.Errorf("ReferrerDomain = %q, want %q", out.ReferrerDomain, c.want)
			}
		})
	}
}

func TestDeriveScreenSize(t *testing.T) {
	cases := []struct {
		name string
		in   Input
		want string
	}{
		{"both dimensions", Input{ScreenWidth: 1920, ScreenHeight: 1080}, "1920x1080"},
		{"missing height", Input{ScreenWidth: 1920}, ""},
		{"missing width", Input{ScreenHeight: 1080}, ""},
		{"zero dimensions", Input{}, ""},
		{"negative rejected", Input{ScreenWidth: -1, ScreenHeight: 1080}, ""},
		{"client value wins", Input{ScreenSize: "390x844", ScreenWidth: 1920, ScreenHeight: 1080}, "390x844"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if out := Derive(c.in); out.ScreenSize != c.want {
				t.Errorf("ScreenSize = %q, want %q", out.ScreenSize, c.want)
			}
		})
	}
}

func TestNormalizeLocale(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"en-US", "en-US"},
		{"en-us", "en-US"},
		{"EN-US", "en-US"},
		{"en_us", "en-US"},
		{"en", "en"},
		{"EN", "en"},
		{"zh-hans-cn", "zh-Hans-CN"},
		{"ZH_HANS_CN", "zh-Hans-CN"},
		{"es-419", "es-419"},
		{"  en-gb  ", "en-GB"},
	}
	for _, c := range cases {
		if got := NormalizeLocale(c.in); got != c.want {
			t.Errorf("NormalizeLocale(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestDeriveChannelGating pins that channel derives only alongside a valid
// page URL: non-web events (no $url) must carry no channel — not "Direct".
func TestDeriveChannelGating(t *testing.T) {
	if out := Derive(Input{Referrer: "https://www.google.com"}); out.Channel != "" {
		t.Errorf("channel without page url = %q, want empty", out.Channel)
	}
	if out := Derive(Input{URL: "garbage", UTMSource: "google"}); out.Channel != "" {
		t.Errorf("channel with invalid page url = %q, want empty", out.Channel)
	}
	if out := Derive(Input{URL: "https://example.com/"}); out.Channel != ChannelDirect {
		t.Errorf("channel for bare page view = %q, want %q", out.Channel, ChannelDirect)
	}
}

// TestDeriveDoesNotMutateInput guards Derive's purity contract.
func TestDeriveDoesNotMutateInput(t *testing.T) {
	in := Input{URL: "https://example.com/?utm_source=google", Referrer: "https://www.bing.com", Locale: "en-us"}
	orig := in
	Derive(in)
	if in != orig {
		t.Errorf("Derive mutated its input: %+v != %+v", in, orig)
	}
}
