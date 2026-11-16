// Package crawler names the crawler behind a User-Agent: an AI agent from the
// hand-kept table in proto/common/crawlers/v1, or else a search engine or SEO
// tool from the crawler-user-agents list, by the list's own tags.
package crawler

import (
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/pug-sh/pug/internal/botdetect"
	commoncrawlersv1 "github.com/pug-sh/pug/internal/gen/proto/common/crawlers/v1"
)

const (
	// EventKind is a crawler request's kind. Only a private key may send one,
	// and the server builds the stored event from its client.
	EventKind = "crawler_request"
	// DistinctIDPrefix starts every crawler's distinct_id. Reserved like
	// cookieless-: no event or identify may claim it, and a crawler is never
	// listed as a person.
	DistinctIDPrefix = "crawler-"
)

// Category is why a crawler fetches a page, as $crawler_category stores it.
type Category string

const (
	CategoryAnswers  Category = "answers"
	CategorySearch   Category = "search"
	CategoryTraining Category = "training"
	CategorySEO      Category = "seo"
	CategoryOther    Category = "other"
)

const (
	tagSearchEngine = "search-engine"
	tagSEO          = "seo"
)

// Agent is one crawler. An entry from the crawler-user-agents list has no
// Company and no Ranges. Ranges is shared with the table: read it, never write it.
type Agent struct {
	// Name is a table row's first token, or the name bot detection's signal 1
	// records for a list entry.
	Name     string
	Company  string
	Category Category
	// Ranges are the URLs where the operator publishes the agent's IP ranges.
	Ranges []string
}

// DistinctID is the distinct_id a crawler's requests are stored under.
func (a Agent) DistinctID() string {
	return DistinctIDPrefix + strings.ToLower(a.Name)
}

type row struct {
	crawler commoncrawlersv1.Crawler
	agent   Agent
	tokens  []string // lowercased
}

var table = mustLoadTable()

// maxUALen bounds the scan, which runs per event: a few list entries are regexps
// that rescan the whole string per literal hit, about 5 ms at 2 KB. Real crawler
// user agents are under 300 bytes.
const maxUALen = 512

// Match returns the crawler ua belongs to: the table row whose token ua holds
// as a whole word, case-insensitively, the longest token winning, or else the
// first crawler-user-agents entry tagged search-engine or seo that matches.
func Match(ua string) (Agent, bool) {
	if len(ua) > maxUALen {
		// Drop the word the bound cuts, or "GPTBotX" cut after its T reads as GPTBot.
		cut := maxUALen
		for cut > 0 && isWordByte(ua[cut]) {
			cut--
		}
		ua = ua[:cut]
	}
	lower := strings.ToLower(ua)
	best, bestLen := -1, 0
	for i, r := range table {
		for _, t := range r.tokens {
			if len(t) > bestLen && containsWord(lower, t) {
				best, bestLen = i, len(t)
			}
		}
	}
	if best >= 0 {
		return table[best].agent, true
	}
	for name, tags := range botdetect.Listed(ua) {
		// The list matches substrings, so its Googlebot entry also takes
		// "NotGooglebot", which the table's whole-word rule refused.
		if namesTableAgent(name) {
			continue
		}
		switch {
		case slices.Contains(tags, tagSearchEngine):
			return Agent{Name: name, Category: CategorySearch}, true
		case slices.Contains(tags, tagSEO):
			return Agent{Name: name, Category: CategorySEO}, true
		}
	}
	return Agent{}, false
}

func namesTableAgent(name string) bool {
	name = strings.ToLower(name)
	for _, r := range table {
		for _, t := range r.tokens {
			if strings.Contains(name, t) {
				return true
			}
		}
	}
	return false
}

// RangeURLs returns every IP-range URL in the table, once each.
func RangeURLs() []string {
	var urls []string
	for _, r := range table {
		for _, u := range r.agent.Ranges {
			if !slices.Contains(urls, u) {
				urls = append(urls, u)
			}
		}
	}
	return urls
}

// containsWord reports whether s holds token with no letter, digit or
// underscore on either side, so "googlebot" is found in "googlebot-image/1.0"
// but not in "notgooglebot".
func containsWord(s, token string) bool {
	for from := 0; ; {
		i := strings.Index(s[from:], token)
		if i < 0 {
			return false
		}
		start := from + i
		end := start + len(token)
		if (start == 0 || !isWordByte(s[start-1])) && (end == len(s) || !isWordByte(s[end])) {
			return true
		}
		from = start + 1
	}
}

func isWordByte(b byte) bool {
	return b == '_' || '0' <= b && b <= '9' || 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z'
}

// mustLoadTable reads the table out of the Crawler enum's options. A row
// without a token or a category is a build mistake the tests catch, so it panics
// rather than serve a table that misnames crawlers.
func mustLoadTable() []row {
	values := commoncrawlersv1.Crawler(0).Descriptor().Values()
	rows := make([]row, 0, values.Len())
	for i := range values.Len() {
		v := values.Get(i)
		if v.Number() == 0 {
			continue
		}
		opts, _ := v.Options().(*descriptorpb.EnumValueOptions)
		if opts.GetDeprecated() {
			continue
		}
		tokens, _ := proto.GetExtension(opts, commoncrawlersv1.E_Token).([]string)
		if len(tokens) == 0 {
			panic(fmt.Sprintf("crawler: %s has no token", v.Name()))
		}
		pb, _ := proto.GetExtension(opts, commoncrawlersv1.E_Category).(commoncrawlersv1.Category)
		category, ok := categoryOf(pb)
		if !ok {
			panic(fmt.Sprintf("crawler: %s has no category", v.Name()))
		}
		company, _ := proto.GetExtension(opts, commoncrawlersv1.E_Company).(string)
		ranges, _ := proto.GetExtension(opts, commoncrawlersv1.E_IpRanges).([]string)
		lower := make([]string, len(tokens))
		for j, t := range tokens {
			lower[j] = strings.ToLower(t)
		}
		rows = append(rows, row{
			crawler: commoncrawlersv1.Crawler(v.Number()),
			agent:   Agent{Name: tokens[0], Company: company, Category: category, Ranges: ranges},
			tokens:  lower,
		})
	}
	return rows
}

func categoryOf(c commoncrawlersv1.Category) (Category, bool) {
	switch c {
	case commoncrawlersv1.Category_CATEGORY_ANSWERS:
		return CategoryAnswers, true
	case commoncrawlersv1.Category_CATEGORY_SEARCH:
		return CategorySearch, true
	case commoncrawlersv1.Category_CATEGORY_TRAINING:
		return CategoryTraining, true
	case commoncrawlersv1.Category_CATEGORY_SEO:
		return CategorySEO, true
	case commoncrawlersv1.Category_CATEGORY_OTHER:
		return CategoryOther, true
	default:
		return "", false
	}
}
