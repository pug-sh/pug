package seed

import (
	"context"
	"fmt"
	"hash/crc64"
	"log/slog"
	"math"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
	"github.com/pug-sh/pug/internal/attribution"
	"github.com/pug-sh/pug/internal/autoprop"
	chq "github.com/pug-sh/pug/internal/core/clickhouse"
	"github.com/pug-sh/pug/internal/crawler"
	"github.com/pug-sh/pug/internal/crawlerips"
	chdeps "github.com/pug-sh/pug/internal/deps/clickhouse"
)

// Crawler requests are what a site's server reports through @pug-sh/crawlers.
// They have no users or profiles, so they get their own backfill and live
// loop. docs/architecture/demo.md ("Crawlers") has the plan.

type demoCrawler struct {
	ua     string
	perDay float64
	// failed is the share of requests whose IP check fails: scrapers borrowing the name.
	failed float64
	// A training crawler bursts for crawlerBurstLen every `every`, starting `at` into its cycle.
	every, at time.Duration

	agent crawler.Agent
}

const (
	crawlerBurstLen      = 6 * time.Hour
	crawlerBurstRequests = 1200

	aiMonthlyGrowth = 1.08
	// notModifiedShare of a search engine's requests re-check a page it holds.
	notModifiedShare = 1.0 / 6

	// The Problems tab's failures: AI labs' crawlers ask for an /llms.txt the
	// store doesn't have, old links still reach a retired product, SEO tools get
	// rate limited, and a training burst overloads the store.
	llmsTxtShare     = 0.02
	rateLimitedShare = 0.01
	overloadedShare  = 0.03
)

const (
	llmsTxtPath    = "/llms.txt"
	retiredProduct = "/products/squeaky-duck-classic"
)

var aiLabs = map[string]bool{"OpenAI": true, "Anthropic": true, "Perplexity": true}

const oneDay = 24 * time.Hour

var demoCrawlers = resolveCrawlers([]demoCrawler{
	{ua: "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko); compatible; ChatGPT-User/1.0; +https://openai.com/bot", perDay: 250},
	{ua: "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; Perplexity-User/1.0; +https://perplexity.ai/perplexity-user)", perDay: 80},
	{ua: "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; Claude-User/1.0; +Claude-User@anthropic.com)", perDay: 50},
	{ua: "meta-externalfetcher/1.1 (+https://developers.facebook.com/docs/sharing/webmasters/crawler)", perDay: 25},
	{ua: "DuckAssistBot/1.2; (+http://duckduckgo.com/duckassistbot.html)", perDay: 20},
	{ua: "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; MistralAI-User/1.0; +https://docs.mistral.ai/robots)", perDay: 15},

	{ua: "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)", perDay: 700, failed: 0.03},
	{ua: "Mozilla/5.0 (compatible; bingbot/2.0; +http://www.bing.com/bingbot.htm)", perDay: 300},
	{ua: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Safari/605.1.15 (Applebot/0.1; +http://www.apple.com/go/applebot)", perDay: 120},
	{ua: "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko); compatible; OAI-SearchBot/1.0; +https://openai.com/searchbot", perDay: 120},
	{ua: "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; PerplexityBot/1.0; +https://perplexity.ai/perplexitybot)", perDay: 80},
	{ua: "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; Claude-SearchBot/1.0; +Claude-SearchBot@anthropic.com)", perDay: 50},
	{ua: "Mozilla/5.0 (Linux; Android 7.0;) AppleWebKit/537.36 (KHTML, like Gecko) Mobile Safari/537.36 (compatible; PetalBot;+https://webmaster.petalsearch.com/site/petalbot)", perDay: 30},

	{ua: "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; GPTBot/1.2; +https://openai.com/gptbot)", perDay: 150, failed: 0.02, every: 16 * oneDay, at: 4*oneDay + 9*time.Hour},
	{ua: "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; ClaudeBot/1.0; +claudebot@anthropic.com)", perDay: 120, every: 23 * oneDay, at: 15*oneDay + 20*time.Hour},
	{ua: "meta-externalagent/1.1 (+https://developers.facebook.com/docs/sharing/webmasters/crawler)", perDay: 120, every: 9 * oneDay, at: 2*oneDay + 3*time.Hour},
	{ua: "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; Amazonbot/0.1; +https://developer.amazon.com/support/amazonbot) Chrome/119.0.6045.214 Safari/537.36", perDay: 60},
	{ua: "Mozilla/5.0 (Linux; Android 5.0) AppleWebKit/537.36 (KHTML, like Gecko) Mobile Safari/537.36 (compatible; Bytespider; spider-feedback@bytedance.com)", perDay: 60, every: 11 * oneDay, at: 6*oneDay + 14*time.Hour},
	{ua: "CCBot/2.0 (https://commoncrawl.org/faq/)", perDay: 30, every: 30 * oneDay, at: 21*oneDay + time.Hour},

	{ua: "Mozilla/5.0 (compatible; AhrefsBot/7.0; +http://ahrefs.com/robot/)", perDay: 300},
	{ua: "Mozilla/5.0 (compatible; SemrushBot/7~bl; +http://www.semrush.com/bot.html)", perDay: 250},
	{ua: "Mozilla/5.0 (compatible; MJ12bot/v1.4.8; http://mj12bot.com/)", perDay: 100},
	{ua: "Mozilla/5.0 (compatible; DotBot/1.2; +https://opensiteexplorer.org/dotbot; help@moz.com)", perDay: 80},
	{ua: "Mozilla/5.0 (compatible; DataForSeoBot/1.0; +https://dataforseo.com/dataforseo-bot)", perDay: 50},

	{ua: "Mozilla/5.0 (Linux; Android 6.0.1; Nexus 5X Build/MMB29P) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.7390.122 Mobile Safari/537.36 (compatible; GoogleOther)", perDay: 60},
})

func resolveCrawlers(crawlers []demoCrawler) []demoCrawler {
	for i := range crawlers {
		agent, ok := crawler.Match(crawlers[i].ua)
		if !ok {
			panic("seed: no crawler matches " + crawlers[i].ua)
		}
		crawlers[i].agent = agent
	}
	return crawlers
}

// meanTrafficFactor is TrafficFactor's average over a week without episodes, so
// a rate scaled by TrafficFactor/meanTrafficFactor keeps its daily total.
var meanTrafficFactor = func() float64 {
	var sum float64
	for h := range 7 * 24 {
		t := userJoinEpoch.Add(time.Duration(h) * time.Hour)
		sum += TrafficFactor(t) / episodeTrafficMult(t)
	}
	return sum / (7 * 24)
}()

// perHour is c's request rate in the hour starting at t. Answers follow the
// shoppers' day, since a person asked for each one.
func (c *demoCrawler) perHour(t, growthUntil time.Time) float64 {
	rate := c.perDay / 24
	if c.agent.Category == crawler.CategoryAnswers {
		rate *= TrafficFactor(t) / meanTrafficFactor * aiGrowth(t, growthUntil)
	}
	if c.bursting(t) {
		rate += crawlerBurstRequests / crawlerBurstLen.Hours()
	}
	return rate
}

func (c *demoCrawler) bursting(t time.Time) bool {
	if c.every == 0 {
		return false
	}
	offset := t.Sub(userJoinEpoch) % c.every
	if offset < 0 {
		offset += c.every
	}
	return offset >= c.at && offset < c.at+crawlerBurstLen
}

// aiGrowth scales AI answers and AI visits: about 8% a month until `until`, then
// flat, so live traffic levels off. A zero until never grows.
func aiGrowth(t, until time.Time) float64 {
	if !t.Before(until) {
		return 1
	}
	return math.Pow(aiMonthlyGrowth, -until.Sub(t).Hours()/(24*30.44))
}

// request is one request by c at occur, drawing its page, verdict and status from r.
func (c *demoCrawler) request(r *rand.Rand, projectID, eventID string, occur time.Time) event {
	pages := crawlerPages()[c.agent.Category]
	path := pages.paths[weightedIndexR(r, pages.weights)]
	if aiLabs[c.agent.Company] && r.Float64() < llmsTxtShare {
		path = llmsTxtPath
	}
	verdict := crawlerips.Unchecked
	if len(c.agent.Ranges) > 0 {
		verdict = crawlerips.Verified
	}
	if r.Float64() < c.failed {
		verdict = crawlerips.Failed
	}
	status := int64(200)
	switch {
	case path == llmsTxtPath || path == retiredProduct:
		status = 404
	case c.bursting(occur) && r.Float64() < overloadedShare:
		status = 503
	case c.agent.Category == crawler.CategorySEO && r.Float64() < rateLimitedShare:
		status = 429
	case c.agent.Category == crawler.CategorySearch && r.Float64() < notModifiedShare:
		status = 304
	}
	return event{
		eventID:        eventID,
		distinctID:     c.agent.DistinctID(),
		sessionID:      crawler.SessionID(projectID, c.agent.Name, occur),
		kind:           crawler.EventKind,
		occurTime:      occur,
		autoProperties: crawlerRequestProps(c.agent, verdict, storeURL+path, status),
	}
}

// crawlerRequestProps builds a crawler request's auto properties the way the
// events handler's asCrawlerEvent does.
func crawlerRequestProps(agent crawler.Agent, verdict crawlerips.Verdict, rawURL string, status int64) map[string]any {
	page := rawURL
	if i := strings.IndexAny(page, "?#"); i >= 0 {
		page = page[:i]
	}
	derived := attribution.Derive(attribution.Input{URL: page})
	props := map[string]any{
		attribution.PropURL:          page,
		autoprop.PropBot:             true,
		autoprop.PropBotReason:       agent.Name,
		autoprop.PropCrawlerCategory: string(agent.Category),
		autoprop.PropCrawlerVerified: string(verdict),
	}
	if derived.Pathname != "" {
		props[attribution.PropPathname] = derived.Pathname
	}
	if derived.Hostname != "" {
		props[attribution.PropHostname] = derived.Hostname
	}
	if agent.Company != "" {
		props[autoprop.PropCrawlerCompany] = agent.Company
	}
	if status >= 100 && status <= 599 {
		props[autoprop.PropStatusCode] = status
	}
	return props
}

type weightedPaths struct {
	paths   []string
	weights []int
}

func (w *weightedPaths) add(path string, weight int) {
	w.paths = append(w.paths, path)
	w.weights = append(w.weights, weight)
}

// crawlerPages is where each purpose's crawlers go. None fetch the cart,
// checkout, account or search pages, which a store's robots.txt disallows.
// Built on first use because the catalog fills in at init.
var crawlerPages = sync.OnceValue(func() map[crawler.Category]*weightedPaths {
	featured := map[string]bool{}
	for _, s := range aiFeaturedPages {
		featured[stepPath(s)] = true
	}
	content := []string{"/"}
	for _, c := range categories {
		content = append(content, "/collections/"+c)
	}
	for _, p := range catalog {
		content = append(content, "/products/"+p.slug)
	}
	content = append(content, "/help")
	for _, a := range helpArticles {
		content = append(content, "/help/"+a.id)
	}
	content = append(content, "/club")

	answers, search, everything := &weightedPaths{}, &weightedPaths{}, &weightedPaths{}
	for _, path := range content {
		var a, s int
		switch {
		case strings.HasPrefix(path, "/products/"):
			a, s = 4, 10
		case strings.HasPrefix(path, "/help/"):
			a, s = 6, 8
		case strings.HasPrefix(path, "/collections/"):
			a, s = 3, 15
		case path == "/":
			a, s = 10, 60
		default:
			a, s = 5, 12
		}
		if featured[path] {
			a = 40
		}
		answers.add(path, a)
		search.add(path, s)
		everything.add(path, 10)
	}
	everything.add("/robots.txt", 20)
	everything.add(retiredProduct, 4)
	search.add("/robots.txt", 25)
	search.add("/sitemap.xml", 15)
	search.add(retiredProduct, 6)
	return map[crawler.Category]*weightedPaths{
		crawler.CategoryAnswers:  answers,
		crawler.CategorySearch:   search,
		crawler.CategoryTraining: everything,
		crawler.CategorySEO:      everything,
		crawler.CategoryOther:    everything,
	}
})

// crawlerEventNamespace scopes the backfill's event ids.
var crawlerEventNamespace = uuid.MustParse("6fe0caba-087e-4c12-8a05-f0880a7f740a")

// crawlerDay returns the crawler requests of the UTC day starting at dayStart
// that fall before until. It draws from a source seeded by the project and the
// day and derives event ids the same way, so a day made twice is the same rows.
func crawlerDay(projectID string, dayStart, until, growthUntil time.Time) []event {
	seed := crc64.Checksum([]byte(projectID), crc64.MakeTable(crc64.ECMA))
	r := rand.New(rand.NewPCG(seed, uint64(dayStart.Unix())))
	date := dayStart.Format(time.DateOnly)

	var out []event
	n := 0
	for h := range 24 {
		hour := dayStart.Add(time.Duration(h) * time.Hour)
		for i := range demoCrawlers {
			c := &demoCrawlers[i]
			rate := c.perHour(hour, growthUntil)
			for at := r.ExpFloat64() / rate; at < 1; at += r.ExpFloat64() / rate {
				occur := hour.Add(time.Duration(at * float64(time.Hour)))
				id := uuid.NewSHA1(crawlerEventNamespace, fmt.Appendf(nil, "%s|%s|%d", projectID, date, n)).String()
				n++
				e := c.request(r, projectID, id, occur)
				if occur.Before(until) {
					out = append(out, e)
				}
			}
		}
	}
	return out
}

// crawlerStore is where the crawler backfill finds its progress and writes its days.
type crawlerStore interface {
	earliestCrawlerRequest(ctx context.Context, projectID string) (time.Time, bool, error)
	insertEvents(ctx context.Context, projectID string, events []event) error
}

// backfillCrawlerRequests fills the UTC days from `from` to now, newest first,
// one day per insert. It carries on before the earliest request stored, so a
// run that stopped part-way resumes without repeating or skipping a day.
func backfillCrawlerRequests(ctx context.Context, store crawlerStore, projectID string, growthUntil, from, now time.Time) (int, error) {
	start := utcDay(from)
	dayStart, until := utcDay(now), now
	earliest, ok, err := store.earliestCrawlerRequest(ctx, projectID)
	if err != nil {
		return 0, fmt.Errorf("find earliest crawler request: %w", err)
	}
	if ok {
		until = utcDay(earliest)
		dayStart = until.Add(-oneDay)
	}
	inserted := 0
	for ; !dayStart.Before(start); dayStart = dayStart.Add(-oneDay) {
		if err := ctx.Err(); err != nil {
			return inserted, err
		}
		events := crawlerDay(projectID, dayStart, until, growthUntil)
		if len(events) > 0 {
			if err := store.insertEvents(ctx, projectID, events); err != nil {
				return inserted, fmt.Errorf("insert crawler requests of %s: %w", dayStart.Format(time.DateOnly), err)
			}
		}
		inserted += len(events)
		until = dayStart
	}
	return inserted, nil
}

func utcDay(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// BackfillCrawlerRequests gives projectID crawler requests over the human
// backfill's window. Run it after the human backfill, whose check counts every
// event. growthUntil is the project's create time.
func BackfillCrawlerRequests(ctx context.Context, ch driver.Conn, projectID string, growthUntil time.Time) error {
	started := time.Now()
	n, err := backfillCrawlerRequests(ctx, chStore{ch: ch}, projectID, growthUntil, backfillFrom(started), started)
	if err != nil {
		return fmt.Errorf("backfill crawler requests: %w", err)
	}
	slog.InfoContext(ctx, "crawler request backfill done",
		slog.String("project_id", projectID),
		slog.Int("inserted", n),
		slog.String("elapsed", time.Since(started).Round(time.Second).String()),
	)
	return nil
}

type chStore struct {
	ch driver.Conn
}

func (s chStore) earliestCrawlerRequest(ctx context.Context, projectID string) (time.Time, bool, error) {
	var (
		earliest time.Time
		n        uint64
	)
	if err := s.ch.QueryRow(ctx,
		"SELECT min(occur_time), count() FROM events WHERE project_id = ? AND kind = ?",
		projectID, crawler.EventKind,
	).Scan(&earliest, &n); err != nil {
		return time.Time{}, false, err
	}
	return earliest, n > 0, nil
}

func (s chStore) insertEvents(ctx context.Context, projectID string, events []event) error {
	batch, err := s.ch.PrepareBatch(ctx, chq.EventsInsertStmt)
	if err != nil {
		return fmt.Errorf("prepare events batch: %w", err)
	}
	defer chdeps.AbortUnsent(ctx, batch)
	for _, e := range events {
		if err := batch.Append(eventArgs(ctx, projectID, e, e.occurTime)...); err != nil {
			return fmt.Errorf("append event: %w", err)
		}
	}
	return batch.Send()
}

// CrawlerRequestsPerMinute is the crawlers' combined request rate at now.
func CrawlerRequestsPerMinute(now time.Time) float64 {
	var perHour float64
	for i := range demoCrawlers {
		perHour += demoCrawlers[i].perHour(now, time.Time{})
	}
	return perHour / 60
}

// LiveCrawlerRequest is one crawler request at now, from a crawler drawn by its
// rate at now.
func LiveCrawlerRequest(projectID string, now time.Time) LiveEvent {
	r := rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	rates := make([]float64, len(demoCrawlers))
	var total float64
	for i := range demoCrawlers {
		rates[i] = demoCrawlers[i].perHour(now, time.Time{})
		total += rates[i]
	}
	pick, x := len(rates)-1, r.Float64()*total
	for i, rate := range rates {
		if x < rate {
			pick = i
			break
		}
		x -= rate
	}
	return toLiveEvents([]event{demoCrawlers[pick].request(r, projectID, uuid.NewString(), now)})[0]
}
