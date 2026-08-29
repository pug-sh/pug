// Package botdetect recognises automated visitors that run the browser SDK —
// headless browsers, synthetic monitors, scrapers on cloud IPs — from what the
// server can see: the User-Agent and the origin network. Rationale, false
// positives and the tag-don't-drop policy: docs/architecture/bot-detection.md.
package botdetect

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"

	agents "github.com/monperrus/crawler-user-agents"
)

// datacenterASNs are networks essentially nobody browses from. Kept off on
// purpose: 15169 Google, 13335 Cloudflare, 54113 Fastly, 20940 Akamai (consumer
// proxy and VPN exits) and every commercial VPN.
var datacenterASNs = map[uint64]bool{
	16509:  true, // Amazon (AWS)
	14618:  true, // Amazon (AWS)
	396982: true, // Google Cloud
	8075:   true, // Microsoft (Azure)
	24940:  true, // Hetzner
	14061:  true, // DigitalOcean
	16276:  true, // OVH
	63949:  true, // Linode (Akamai cloud)
	20473:  true, // Vultr
	31898:  true, // Oracle Cloud
	12876:  true, // Scaleway
	51167:  true, // Contabo
	8560:   true, // IONOS
	40509:  true, // Fly.io
	45102:  true, // Alibaba Cloud
	132203: true, // Tencent Cloud
}

// patterns are the list's regexps, compiled once so a hit can report the text
// it matched: Crawler.Pattern is regex source (`Googlebot\/`, `S[eE][mM]rushBot`)
// and must never reach a dashboard.
var patterns = func() []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(agents.Crawlers))
	for i, c := range agents.Crawlers {
		out[i] = regexp.MustCompile(c.Pattern)
	}
	return out
}()

// maxUALen bounds the scan: the list's few regexp-backed patterns rescan the
// whole string per literal hit, so an attacker-sized User-Agent is quadratic.
const maxUALen = 2048

// MatchUserAgent returns the crawler name as it appears in ua when ua matches
// the crawler-user-agents list.
func MatchUserAgent(ua string) (string, bool) {
	if len(ua) > maxUALen {
		ua = ua[:maxUALen]
	}
	hits := agents.MatchingCrawlers(ua)
	if len(hits) == 0 {
		return "", false
	}
	name := patterns[hits[0]].FindString(ua)
	return strings.TrimFunc(name, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }), true
}

// MatchASN returns "asn:<n>" when asn is a datacenter-only network.
func MatchASN(asn uint64) (string, bool) {
	if !datacenterASNs[asn] {
		return "", false
	}
	return "asn:" + strconv.FormatUint(asn, 10), true
}
