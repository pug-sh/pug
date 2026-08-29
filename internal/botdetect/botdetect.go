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

// names are one stable label per list entry (its pattern matched against its
// own example user agent): Crawler.Pattern is regex source, and a span over the
// live User-Agent would be request-chosen text with unbounded cardinality.
var names = func() []string {
	strip := strings.NewReplacer(`\`, "", "^", "", "$", "", "(", "", ")", "")
	out := make([]string, len(agents.Crawlers))
	for i, c := range agents.Crawlers {
		re := regexp.MustCompile(c.Pattern)
		for _, ua := range c.Instances {
			if out[i] = trimName(re.FindString(ua)); out[i] != "" {
				break
			}
		}
		if out[i] == "" {
			out[i] = trimName(strip.Replace(c.Pattern))
		}
	}
	return out
}()

// maxUALen bounds the scan: the list's few regexp-backed patterns rescan the
// whole string per literal hit, so an attacker-sized User-Agent is quadratic.
const maxUALen = 2048

// MatchUserAgent returns the crawler's name when ua matches the
// crawler-user-agents list.
func MatchUserAgent(ua string) (string, bool) {
	if len(ua) > maxUALen {
		ua = ua[:maxUALen]
	}
	hits := agents.MatchingCrawlers(ua)
	if len(hits) == 0 {
		return "", false
	}
	return names[hits[0]], true
}

func trimName(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// MatchASN returns "asn:<n>" when asn is a datacenter-only network.
func MatchASN(asn uint64) (string, bool) {
	if !datacenterASNs[asn] {
		return "", false
	}
	return "asn:" + strconv.FormatUint(asn, 10), true
}
