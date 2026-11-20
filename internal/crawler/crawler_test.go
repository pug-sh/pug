package crawler

import (
	"slices"
	"strings"
	"testing"
	"time"

	agents "github.com/monperrus/crawler-user-agents"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/pug-sh/pug/internal/botdetect"
	commoncrawlersv1 "github.com/pug-sh/pug/internal/gen/proto/common/crawlers/v1"
)

// samples holds one real user agent per row, as its operator documents it.
var samples = map[commoncrawlersv1.Crawler]string{
	commoncrawlersv1.Crawler_CRAWLER_CHATGPT_USER:          "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko); compatible; ChatGPT-User/1.0; +https://openai.com/bot",
	commoncrawlersv1.Crawler_CRAWLER_CLAUDE_USER:           "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; Claude-User/1.0; +Claude-User@anthropic.com)",
	commoncrawlersv1.Crawler_CRAWLER_PERPLEXITY_USER:       "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; Perplexity-User/1.0; +https://perplexity.ai/perplexity-user)",
	commoncrawlersv1.Crawler_CRAWLER_MISTRALAI_USER:        "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; MistralAI-User/1.0; +https://docs.mistral.ai/robots)",
	commoncrawlersv1.Crawler_CRAWLER_KIMI_USER:             "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; Kimi-User/1.0; +https://www.kimi.com/policies/kimi-crawlers)",
	commoncrawlersv1.Crawler_CRAWLER_DUCKASSISTBOT:         "DuckAssistBot/1.2; (+http://duckduckgo.com/duckassistbot.html)",
	commoncrawlersv1.Crawler_CRAWLER_AMZN_USER:             "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; Amzn-User/0.1) Chrome/119.0.6045.214 Safari/537.36",
	commoncrawlersv1.Crawler_CRAWLER_META_EXTERNALFETCHER:  "meta-externalfetcher/1.1 (+https://developers.facebook.com/docs/sharing/webmasters/crawler)",
	commoncrawlersv1.Crawler_CRAWLER_GOOGLE_AGENT:          "Mozilla/5.0 (Linux; Android 6.0.1; Nexus 5X Build/MMB29P) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.7390.122 Mobile Safari/537.36 (compatible; Google-Agent)",
	commoncrawlersv1.Crawler_CRAWLER_GOOGLE_GEMININOTEBOOK: "Google-GeminiNotebook",
	commoncrawlersv1.Crawler_CRAWLER_GOOGLE_READ_ALOUD:     "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.7390.122 Safari/537.36 (compatible; Google-Read-Aloud; +https://support.google.com/webmasters/answer/1061943)",
	commoncrawlersv1.Crawler_CRAWLER_OAI_SEARCHBOT:         "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko); compatible; OAI-SearchBot/1.0; +https://openai.com/searchbot",
	commoncrawlersv1.Crawler_CRAWLER_CLAUDE_SEARCHBOT:      "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; Claude-SearchBot/1.0; +Claude-SearchBot@anthropic.com)",
	commoncrawlersv1.Crawler_CRAWLER_PERPLEXITYBOT:         "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; PerplexityBot/1.0; +https://perplexity.ai/perplexitybot)",
	commoncrawlersv1.Crawler_CRAWLER_MISTRALAI_INDEX:       "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; MistralAI-Index/1.0; +https://docs.mistral.ai/robots)",
	commoncrawlersv1.Crawler_CRAWLER_KIMI_SEARCHBOT:        "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; Kimi-SearchBot/1.0; +https://www.kimi.com/policies/kimi-crawlers)",
	commoncrawlersv1.Crawler_CRAWLER_AMZN_SEARCHBOT:        "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; Amzn-SearchBot/0.1) Chrome/119.0.6045.214 Safari/537.36",
	commoncrawlersv1.Crawler_CRAWLER_META_WEBINDEXER:       "meta-webindexer/1.1 (+https://developers.facebook.com/docs/sharing/webmasters/crawler)",
	commoncrawlersv1.Crawler_CRAWLER_GOOGLEBOT:             "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)",
	commoncrawlersv1.Crawler_CRAWLER_BINGBOT:               "Mozilla/5.0 (compatible; bingbot/2.0; +http://www.bing.com/bingbot.htm)",
	commoncrawlersv1.Crawler_CRAWLER_APPLEBOT:              "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Safari/605.1.15 (Applebot/0.1; +http://www.apple.com/go/applebot)",
	commoncrawlersv1.Crawler_CRAWLER_BAIDUSPIDER:           "Mozilla/5.0 (compatible; Baiduspider/2.0; +http://www.baidu.com/search/spider.html)",
	commoncrawlersv1.Crawler_CRAWLER_YOUBOT:                "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; YouBot (+http://about.you.com/youbot))",
	commoncrawlersv1.Crawler_CRAWLER_GPTBOT:                "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; GPTBot/1.2; +https://openai.com/gptbot)",
	commoncrawlersv1.Crawler_CRAWLER_CLAUDEBOT:             "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; ClaudeBot/1.0; +claudebot@anthropic.com)",
	commoncrawlersv1.Crawler_CRAWLER_META_EXTERNALAGENT:    "meta-externalagent/1.1 (+https://developers.facebook.com/docs/sharing/webmasters/crawler)",
	commoncrawlersv1.Crawler_CRAWLER_AMAZONBOT:             "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; Amazonbot/0.1; +https://developer.amazon.com/support/amazonbot) Chrome/119.0.6045.214 Safari/537.36",
	commoncrawlersv1.Crawler_CRAWLER_KIMIBOT:               "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; KimiBot/1.0; +https://www.kimi.com/policies/kimi-crawlers)",
	commoncrawlersv1.Crawler_CRAWLER_CCBOT:                 "CCBot/2.0 (https://commoncrawl.org/faq/)",
	commoncrawlersv1.Crawler_CRAWLER_BYTESPIDER:            "Mozilla/5.0 (Linux; Android 5.0) AppleWebKit/537.36 (KHTML, like Gecko) Mobile Safari/537.36 (compatible; Bytespider; spider-feedback@bytedance.com)",
	commoncrawlersv1.Crawler_CRAWLER_AI2BOT:                "Mozilla/5.0 (compatible) AI2Bot (+https://www.allenai.org/crawler)",
	commoncrawlersv1.Crawler_CRAWLER_OAI_ADSBOT:            "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko); compatible; OAI-AdsBot/1.0; +https://openai.com/adsbot",
	commoncrawlersv1.Crawler_CRAWLER_META_EXTERNALADS:      "meta-externalads/1.1 (+https://developers.facebook.com/docs/sharing/webmasters/crawler)",
	commoncrawlersv1.Crawler_CRAWLER_GOOGLEOTHER:           "Mozilla/5.0 (Linux; Android 6.0.1; Nexus 5X Build/MMB29P) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.7390.122 Mobile Safari/537.36 (compatible; GoogleOther)",
	commoncrawlersv1.Crawler_CRAWLER_GOOGLE_CLOUDVERTEXBOT: "Mozilla/5.0 (compatible; Google-CloudVertexBot; +https://cloud.google.com/enterprise-search)",
}

func options(c commoncrawlersv1.Crawler) *descriptorpb.EnumValueOptions {
	opts, _ := c.Descriptor().Values().ByNumber(c.Number()).Options().(*descriptorpb.EnumValueOptions)
	return opts
}

func TestTableSize(t *testing.T) {
	if len(table) != 35 {
		t.Errorf("table has %d rows, want 35", len(table))
	}
	withRanges := 0
	for _, r := range table {
		if len(r.agent.Ranges) > 0 {
			withRanges++
		}
	}
	if withRanges != 27 {
		t.Errorf("%d rows can be checked by IP, want 27", withRanges)
	}
	if n := len(RangeURLs()); n != 23 {
		t.Errorf("the table names %d range files, want 23", n)
	}
}

func TestTokensAreUnique(t *testing.T) {
	seen := map[string]commoncrawlersv1.Crawler{}
	for _, r := range table {
		for _, tok := range r.tokens {
			if other, dup := seen[tok]; dup {
				t.Errorf("token %q is in both %s and %s", tok, other, r.crawler)
			}
			seen[tok] = r.crawler
		}
	}
}

func TestEveryRowHasASource(t *testing.T) {
	for _, r := range table {
		opts := options(r.crawler)
		page, _ := proto.GetExtension(opts, commoncrawlersv1.E_OperatorPage).(string)
		lists, _ := proto.GetExtension(opts, commoncrawlersv1.E_ListedBy).([]string)
		switch {
		case page != "":
			if !strings.HasPrefix(page, "https://") {
				t.Errorf("%s: operator page %q is not https", r.crawler, page)
			}
		case len(lists) < 2:
			t.Errorf("%s: no operator page and %d list names, want two", r.crawler, len(lists))
		}
		if r.agent.Company == "" {
			t.Errorf("%s: no company", r.crawler)
		}
		for _, u := range r.agent.Ranges {
			if !strings.HasPrefix(u, "https://") {
				t.Errorf("%s: range URL %q is not https", r.crawler, u)
			}
		}
		for _, l := range lists {
			if l != "crawler-user-agents" {
				continue
			}
			named := false
			for range botdetect.Listed(samples[r.crawler]) {
				named = true
			}
			if !named {
				t.Errorf("%s: listed_by crawler-user-agents, but the list doesn't match its sample", r.crawler)
			}
		}
	}
}

func TestEachSampleMatchesItsRowAlone(t *testing.T) {
	for _, r := range table {
		ua, ok := samples[r.crawler]
		if !ok {
			t.Errorf("%s: no sample user agent", r.crawler)
			continue
		}
		got, ok := Match(ua)
		if !ok || got.Name != r.agent.Name || got.Company != r.agent.Company || got.Category != r.agent.Category {
			t.Errorf("%s: Match(%q) = %+v, %v; want %+v", r.crawler, ua, got, ok, r.agent)
		}
		lower := strings.ToLower(ua)
		for _, other := range table {
			if other.crawler == r.crawler {
				continue
			}
			for _, tok := range other.tokens {
				if containsWord(lower, tok) {
					t.Errorf("%s: its sample also holds %s's token %q", r.crawler, other.crawler, tok)
				}
			}
		}
	}
}

func TestMatch(t *testing.T) {
	tests := []struct {
		name string
		ua   string
		want Agent
		ok   bool
	}{
		// The list tags all three, but the table's rows win.
		{"Googlebot keeps its row", samples[commoncrawlersv1.Crawler_CRAWLER_GOOGLEBOT], agentOf(commoncrawlersv1.Crawler_CRAWLER_GOOGLEBOT), true},
		{"OAI-SearchBot keeps its row", samples[commoncrawlersv1.Crawler_CRAWLER_OAI_SEARCHBOT], agentOf(commoncrawlersv1.Crawler_CRAWLER_OAI_SEARCHBOT), true},
		{"Amazonbot stays training", samples[commoncrawlersv1.Crawler_CRAWLER_AMAZONBOT], agentOf(commoncrawlersv1.Crawler_CRAWLER_AMAZONBOT), true},

		{"a token as a prefix of a longer name", "Googlebot-Image/1.0", agentOf(commoncrawlersv1.Crawler_CRAWLER_GOOGLEBOT), true},
		{"any case", "mozilla/5.0 (compatible; gptbot/1.2)", agentOf(commoncrawlersv1.Crawler_CRAWLER_GPTBOT), true},
		{"a row's second token", "Mozilla/5.0 (compatible; Google-NotebookLM)", agentOf(commoncrawlersv1.Crawler_CRAWLER_GOOGLE_GEMININOTEBOOK), true},
		{"msnbot is bingbot", "msnbot/2.0b (+http://search.msn.com/msnbot.htm)", agentOf(commoncrawlersv1.Crawler_CRAWLER_BINGBOT), true},
		{"the longest token wins", "Mozilla/5.0 (compatible; Googlebot; Google-CloudVertexBot)", agentOf(commoncrawlersv1.Crawler_CRAWLER_GOOGLE_CLOUDVERTEXBOT), true},

		{"seo from the list", "Mozilla/5.0 (compatible; AhrefsBot/7.0; +http://ahrefs.com/robot/)", Agent{Name: "AhrefsBot", Category: CategorySEO}, true},
		{"search from the list", "Mozilla/5.0 (compatible; Google-InspectionTool/1.0;)", Agent{Name: "Google-InspectionTool", Category: CategorySearch}, true},

		{"not as part of a word", "NotGooglebot/1.0", Agent{}, false},
		{"a list entry for a table agent never stands in for it", "Mozilla/5.0 (compatible; XGooglebot-Image/1.0)", Agent{}, false},
		{"a browser", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36", Agent{}, false},
		{"an HTTP library", "curl/8.4.0", Agent{}, false},
		{"a monitor", "Mozilla/5.0+(compatible; UptimeRobot/2.0; http://www.uptimerobot.com/)", Agent{}, false},
		{"a link preview", "facebookexternalhit/1.1 (+http://www.facebook.com/externalhit_uatext.php)", Agent{}, false},
		{"past the scan bound", strings.Repeat("x", maxUALen) + " GPTBot/1.2", Agent{}, false},
		{"a token ending at the scan bound", strings.Repeat("x", maxUALen-7) + " GPTBot/1.2", agentOf(commoncrawlersv1.Crawler_CRAWLER_GPTBOT), true},
		{"a longer word cut at the scan bound", strings.Repeat("x", maxUALen-7) + " GPTBotX/1.2", Agent{}, false},
		{"empty", "", Agent{}, false},
	}
	for _, tt := range tests {
		got, ok := Match(tt.ua)
		if ok != tt.ok || got.Name != tt.want.Name || got.Company != tt.want.Company || got.Category != tt.want.Category {
			t.Errorf("%s: Match(%q) = %+v, %v; want %+v, %v", tt.name, tt.ua, got, ok, tt.want, tt.ok)
		}
	}
}

func agentOf(c commoncrawlersv1.Crawler) Agent {
	for _, r := range table {
		if r.crawler == c {
			return r.agent
		}
	}
	panic("no row for " + c.String())
}

// A list bump must not leave a search engine or SEO tool unmatched or nameless.
func TestEveryListedSearchAndSEOAgentMatches(t *testing.T) {
	for _, c := range agents.Crawlers {
		if len(c.Instances) == 0 || !slices.Contains(c.Tags, tagSearchEngine) && !slices.Contains(c.Tags, tagSEO) {
			continue
		}
		if a, ok := Match(c.Instances[0]); !ok || a.Name == "" {
			t.Errorf("%q: Match(%q) = %+v, %v", c.Pattern, c.Instances[0], a, ok)
		}
	}
}

func TestDistinctID(t *testing.T) {
	for _, tt := range []struct {
		agent Agent
		want  string
	}{
		{agentOf(commoncrawlersv1.Crawler_CRAWLER_GPTBOT), "crawler-gptbot"},
		{agentOf(commoncrawlersv1.Crawler_CRAWLER_CHATGPT_USER), "crawler-chatgpt-user"},
		{Agent{Name: "AhrefsBot"}, "crawler-ahrefsbot"},
	} {
		if got := tt.agent.DistinctID(); got != tt.want {
			t.Errorf("%s: DistinctID() = %q, want %q", tt.agent.Name, got, tt.want)
		}
	}
}

func TestSessionIDIsOnePerAgentPerDay(t *testing.T) {
	occur := time.Date(2026, 11, 14, 9, 30, 0, 0, time.UTC)
	base := SessionID("proj-1", "GPTBot", occur)
	if base != "b586e989-2f88-537e-a6e5-e580fa3af00d" {
		t.Errorf("SessionID = %q; stored crawler sessions would split", base)
	}
	if SessionID("proj-1", "GPTBot", occur.Add(14*time.Hour)) != base {
		t.Error("two requests on one UTC day got two sessions")
	}
	for name, other := range map[string]string{
		"next UTC day":  SessionID("proj-1", "GPTBot", occur.Add(15*time.Hour)),
		"another agent": SessionID("proj-1", "ClaudeBot", occur),
		"another site":  SessionID("proj-2", "GPTBot", occur),
	} {
		if other == base {
			t.Errorf("%s shares the session", name)
		}
	}
	if SessionID("proj-1", "GPTBot", occur.In(time.FixedZone("IST", 5*3600+1800))) != base {
		t.Error("the session day is not taken in UTC")
	}
}

func TestRangeURLsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, u := range RangeURLs() {
		if seen[u] {
			t.Errorf("%s listed twice", u)
		}
		seen[u] = true
	}
}
