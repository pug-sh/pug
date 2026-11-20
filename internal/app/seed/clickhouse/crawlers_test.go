package seed

import (
	"context"
	"errors"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pug-sh/pug/internal/crawler"
	"github.com/pug-sh/pug/internal/crawlerips"
	"github.com/pug-sh/pug/internal/testutil"
)

var errStoreDown = errors.New("store down")

type memCrawlerStore struct {
	events []event
}

func (s *memCrawlerStore) earliestCrawlerRequest(context.Context, string) (time.Time, bool, error) {
	if len(s.events) == 0 {
		return time.Time{}, false, nil
	}
	earliest := s.events[0].occurTime
	for _, e := range s.events {
		if e.occurTime.Before(earliest) {
			earliest = e.occurTime
		}
	}
	return earliest, true, nil
}

func (s *memCrawlerStore) insertEvents(_ context.Context, _ string, events []event) error {
	s.events = append(s.events, events...)
	return nil
}

// failingStore lets failAfter inserts through, then fails every one after.
type failingStore struct {
	crawlerStore
	failAfter, inserts int
}

func (s *failingStore) insertEvents(ctx context.Context, projectID string, events []event) error {
	if s.inserts == s.failAfter {
		return errStoreDown
	}
	s.inserts++
	return s.crawlerStore.insertEvents(ctx, projectID, events)
}

func eventIDs(events []event) []string {
	ids := make([]string, len(events))
	for i, e := range events {
		ids[i] = e.eventID
	}
	slices.Sort(ids)
	return ids
}

var testNow = time.Date(2026, 11, 20, 15, 4, 5, 0, time.UTC)

func backfillInMemory(t *testing.T, from, now time.Time) *memCrawlerStore {
	t.Helper()
	store := &memCrawlerStore{}
	if _, err := backfillCrawlerRequests(t.Context(), store, "proj", time.Time{}, from, now); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestDemoCrawlersAreTheDocumentedOnes(t *testing.T) {
	want := []struct {
		name, company string
		category      crawler.Category
	}{
		{"ChatGPT-User", "OpenAI", crawler.CategoryAnswers},
		{"Perplexity-User", "Perplexity", crawler.CategoryAnswers},
		{"Claude-User", "Anthropic", crawler.CategoryAnswers},
		{"meta-externalfetcher", "Meta", crawler.CategoryAnswers},
		{"DuckAssistBot", "DuckDuckGo", crawler.CategoryAnswers},
		{"MistralAI-User", "Mistral", crawler.CategoryAnswers},
		{"Googlebot", "Google", crawler.CategorySearch},
		{"bingbot", "Microsoft", crawler.CategorySearch},
		{"Applebot", "Apple", crawler.CategorySearch},
		{"OAI-SearchBot", "OpenAI", crawler.CategorySearch},
		{"PerplexityBot", "Perplexity", crawler.CategorySearch},
		{"Claude-SearchBot", "Anthropic", crawler.CategorySearch},
		{"PetalBot", "", crawler.CategorySearch},
		{"GPTBot", "OpenAI", crawler.CategoryTraining},
		{"ClaudeBot", "Anthropic", crawler.CategoryTraining},
		{"meta-externalagent", "Meta", crawler.CategoryTraining},
		{"Amazonbot", "Amazon", crawler.CategoryTraining},
		{"Bytespider", "ByteDance", crawler.CategoryTraining},
		{"CCBot", "Common Crawl", crawler.CategoryTraining},
		{"AhrefsBot", "", crawler.CategorySEO},
		{"SemrushBot", "", crawler.CategorySEO},
		{"MJ12bot", "", crawler.CategorySEO},
		{"dotbot", "", crawler.CategorySEO},
		{"DataForSeoBot", "", crawler.CategorySEO},
		{"GoogleOther", "Google", crawler.CategoryOther},
	}
	if len(demoCrawlers) != len(want) {
		t.Fatalf("%d demo crawlers, want %d", len(demoCrawlers), len(want))
	}
	var perDay float64
	for i, c := range demoCrawlers {
		w := want[i]
		if c.agent.Name != w.name || c.agent.Company != w.company || c.agent.Category != w.category {
			t.Errorf("crawler %d matches %s/%q/%s, want %s/%q/%s", i, c.agent.Name, c.agent.Company, c.agent.Category, w.name, w.company, w.category)
		}
		if c.every != 0 && c.agent.Category != crawler.CategoryTraining {
			t.Errorf("%s bursts, but only training crawlers do", c.agent.Name)
		}
		perDay += c.perDay
	}
	if perDay != 3220 {
		t.Errorf("crawlers make %v requests a day without bursts, want 3220", perDay)
	}
}

// TestCrawlerRequestPropsLikeHandler mirrors the events handler's
// TestCrawlerRequestIsStored field for field: same request, same stored
// properties. The demo writes crawler requests straight to ClickHouse, so this
// is what keeps them looking like ingested ones.
func TestCrawlerRequestPropsLikeHandler(t *testing.T) {
	agent, ok := crawler.Match("Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; GPTBot/1.2; +https://openai.com/gptbot)")
	if !ok {
		t.Fatal("GPTBot's user agent matches no crawler")
	}
	got := crawlerRequestProps(agent, crawlerips.Verified, "https://Example.com/pricing/caf%C3%A9?utm_source=x&token=secret#plans", 404)
	want := map[string]any{
		"$url":              "https://Example.com/pricing/caf%C3%A9",
		"$pathname":         "/pricing/café",
		"$hostname":         "example.com",
		"$bot":              true,
		"$bot_reason":       "GPTBot",
		"$crawler_company":  "OpenAI",
		"$crawler_category": "training",
		"$crawler_verified": "verified",
		"$status_code":      int64(404),
	}
	if !maps.EqualFunc(got, want, func(a, b any) bool { return a == b }) {
		t.Errorf("props = %v\nwant    %v", got, want)
	}
	if _, ok := crawlerRequestProps(agent, crawlerips.Verified, "https://example.com/", 0)["$status_code"]; ok {
		t.Error("a status outside 100-599 was stored")
	}
}

func TestCrawlerRequestsLookIngested(t *testing.T) {
	disallowed := []string{"/cart", "/checkout", "/account", "/search", "/signup", "/signin"}
	byName := map[string]*demoCrawler{}
	for i := range demoCrawlers {
		byName[demoCrawlers[i].agent.Name] = &demoCrawlers[i]
	}
	for _, e := range backfillInMemory(t, testNow.AddDate(0, 0, -3), testNow).events {
		props := e.autoProperties
		name, _ := props["$bot_reason"].(string)
		status, _ := props["$status_code"].(int64)
		category, _ := props["$crawler_category"].(string)
		company, _ := props["$crawler_company"].(string)
		path, _ := props["$pathname"].(string)
		missing := path == retiredProduct || path == llmsTxtPath
		var expected bool
		switch status {
		case 200:
			expected = !missing
		case 304:
			expected = !missing && category == string(crawler.CategorySearch)
		case 404:
			expected = missing
		case 429:
			expected = !missing && category == string(crawler.CategorySEO)
		case 503:
			expected = !missing && byName[name].bursting(e.occurTime)
		}
		if !expected || path == llmsTxtPath && !aiLabs[company] {
			t.Fatalf("%s got a %d for %s", name, status, path)
		}
		if slices.ContainsFunc(disallowed, func(p string) bool { return strings.HasPrefix(path, p) }) {
			t.Fatalf("%s fetched %s, which robots.txt disallows", name, path)
		}
		if e.kind != crawler.EventKind || !strings.HasPrefix(e.distinctID, crawler.DistinctIDPrefix) || e.customProperties != nil {
			t.Fatalf("event = kind %q, distinct_id %q, custom %v", e.kind, e.distinctID, e.customProperties)
		}
		if e.sessionID != crawler.SessionID("proj", name, e.occurTime) {
			t.Fatalf("%s's session %q is not ingest's", name, e.sessionID)
		}
		if !e.occurTime.Before(testNow) {
			t.Fatalf("request at %v, after now", e.occurTime)
		}
	}
}

func TestCrawlerFailuresFillProblems(t *testing.T) {
	midnight := utcDay(testNow)
	failures := map[string]int{}
	total := 0
	for _, e := range backfillInMemory(t, midnight.AddDate(0, 0, -14), midnight).events {
		total++
		switch status := e.autoProperties["$status_code"].(int64); status {
		case 404:
			failures[e.autoProperties["$pathname"].(string)]++
		case 429, 503:
			failures[strconv.FormatInt(status, 10)]++
		}
	}
	sum := 0
	for _, kind := range []string{llmsTxtPath, retiredProduct, "429", "503"} {
		if failures[kind] == 0 {
			t.Errorf("no %s failures in two weeks", kind)
		}
		sum += failures[kind]
	}
	if share := float64(sum) / float64(total); share < 0.005 || share > 0.03 {
		t.Errorf("%.1f%% of crawler requests failed, want about 1.5%%", 100*share)
	}
}

func TestCrawlerBackfillResumes(t *testing.T) {
	from := testNow.AddDate(0, 0, -6)
	whole := backfillInMemory(t, from, testNow)

	cut := &memCrawlerStore{}
	if _, err := backfillCrawlerRequests(t.Context(), &failingStore{crawlerStore: cut, failAfter: 3}, "proj", time.Time{}, from, testNow); !errors.Is(err, errStoreDown) {
		t.Fatalf("cut run: err = %v, want the store's error", err)
	}
	if len(cut.events) == 0 {
		t.Fatal("cut run wrote nothing")
	}
	// The restart comes later, but the newest day is already written and stays.
	if _, err := backfillCrawlerRequests(t.Context(), cut, "proj", time.Time{}, from, testNow.Add(time.Hour)); err != nil {
		t.Fatalf("resumed run: %v", err)
	}
	if !slices.Equal(eventIDs(cut.events), eventIDs(whole.events)) {
		t.Errorf("resumed backfill holds %d requests, an uncut one %d, or other ids", len(cut.events), len(whole.events))
	}
	if n, err := backfillCrawlerRequests(t.Context(), cut, "proj", time.Time{}, from, testNow.Add(2*time.Hour)); err != nil || n != 0 {
		t.Errorf("a finished backfill ran again: inserted %d, err %v", n, err)
	}

	earliest, _, _ := whole.earliestCrawlerRequest(t.Context(), "proj")
	if !utcDay(earliest).Equal(utcDay(from)) {
		t.Errorf("backfill starts on %v, want %v", utcDay(earliest), utcDay(from))
	}
	ids := eventIDs(whole.events)
	if len(slices.Compact(ids)) != len(whole.events) {
		t.Error("an event id repeats")
	}
}

func TestCrawlerDayVolume(t *testing.T) {
	midnight := utcDay(testNow)
	store := backfillInMemory(t, midnight.AddDate(0, 0, -28), midnight)
	// 3,220 without bursts, plus about 400 a day of bursts.
	if perDay := float64(len(store.events)) / 28; perDay < 3300 || perDay > 4000 {
		t.Errorf("%.0f crawler requests a day, want about 3,650", perDay)
	}
}

func TestTrainingCrawlersBurst(t *testing.T) {
	var gpt *demoCrawler
	for i := range demoCrawlers {
		if demoCrawlers[i].agent.Name == "GPTBot" {
			gpt = &demoCrawlers[i]
		}
	}
	start := userJoinEpoch.Add(gpt.at)
	if gpt.bursting(start.Add(-time.Minute)) || !gpt.bursting(start) || gpt.bursting(start.Add(crawlerBurstLen)) || !gpt.bursting(start.Add(gpt.every)) {
		t.Error("GPTBot's burst doesn't start at its offset, last six hours and recur on its cycle")
	}
	if burst, base := gpt.perHour(start, time.Time{}), gpt.perHour(start.Add(-time.Hour), time.Time{}); burst < 20*base {
		t.Errorf("GPTBot makes %.0f requests an hour in a burst against %.0f outside one", burst, base)
	}

	bursts := 0
	from := utcDay(testNow).AddDate(0, 0, -90)
	for h := from; h.Before(utcDay(testNow)); h = h.Add(time.Hour) {
		for i := range demoCrawlers {
			if c := &demoCrawlers[i]; c.bursting(h) && !c.bursting(h.Add(-time.Hour)) {
				bursts++
			}
		}
	}
	if bursts < 26 || bursts > 36 {
		t.Errorf("%d bursts in 90 days, want one about every three days", bursts)
	}
}

func TestAIGrowth(t *testing.T) {
	until := time.Date(2026, 11, 7, 19, 33, 58, 0, time.UTC)
	if aiGrowth(until, until) != 1 || aiGrowth(until.Add(time.Hour), until) != 1 {
		t.Error("AI traffic grows past until")
	}
	if g := aiGrowth(until.Add(-time.Duration(30.44*24*float64(time.Hour))), until); math.Abs(g-1/aiMonthlyGrowth) > 1e-9 {
		t.Errorf("a month before until AI traffic is %.4f of its level, want %.4f", g, 1/aiMonthlyGrowth)
	}
	if aiGrowth(testNow, time.Time{}) != 1 {
		t.Error("a zero until grows")
	}
}

func TestLastDayShowsEveryPurpose(t *testing.T) {
	since := testNow.Add(-24 * time.Hour)
	purposes := map[string]bool{}
	failed := 0
	for _, e := range backfillInMemory(t, since, testNow).events {
		if e.occurTime.Before(since) {
			continue
		}
		category, _ := e.autoProperties["$crawler_category"].(string)
		purposes[category] = true
		if e.autoProperties["$crawler_verified"] == string(crawlerips.Failed) {
			failed++
		}
	}
	for _, p := range []crawler.Category{crawler.CategoryAnswers, crawler.CategorySearch, crawler.CategoryTraining, crawler.CategorySEO} {
		if !purposes[string(p)] {
			t.Errorf("no %s requests in the last 24 hours", p)
		}
	}
	if failed == 0 {
		t.Error("no request failed its IP check in the last 24 hours")
	}

	f := newSessionFactory()
	web, ai, featured := 0, 0, 0
	featuredPaths := map[string]bool{}
	for _, s := range aiFeaturedPages {
		featuredPaths[stepPath(s)] = true
	}
	for range 8000 {
		sess := f.session(t.Context(), since, testNow)
		if len(sess) == 0 || sess[0].autoProperties["$platform"] != "web" {
			continue
		}
		web++
		if sess[0].autoProperties["$channel"] == "AI Assistants" {
			ai++
			if path, _ := sess[0].autoProperties["$pathname"].(string); featuredPaths[path] {
				featured++
			}
		}
	}
	if share := float64(ai) / float64(web); share < 0.015 || share > 0.05 {
		t.Errorf("AI products sent %.1f%% of web sessions, want about 3%%", 100*share)
	}
	if ai > 0 && float64(featured)/float64(ai) < 0.6 {
		t.Errorf("%d of %d AI visits landed on the featured pages, want about three in four", featured, ai)
	}
}

func TestAIVisitsBookToAIAssistants(t *testing.T) {
	want := map[string]string{
		"https://chatgpt.com/":           "chatgpt.com",
		"https://www.perplexity.ai/":     "perplexity.ai",
		"https://gemini.google.com/":     "gemini.google.com",
		"https://claude.ai/":             "claude.ai",
		"https://copilot.microsoft.com/": "copilot.microsoft.com",
	}
	if len(aiReferrals) != len(want) {
		t.Fatalf("%d AI referrers, want %d", len(aiReferrals), len(want))
	}
	for _, a := range aiReferrals {
		props := map[string]any{"$url": storeURL + "/help/harness-sizing", "$referrer": a.referrer}
		if a.utmSource != "" {
			props["$utmSource"] = a.utmSource
		}
		applyAttribution(props)
		if props["$channel"] != "AI Assistants" || props["$referrerDomain"] != want[a.referrer] {
			t.Errorf("%s books to %v from %v, want AI Assistants from %s", a.referrer, props["$channel"], props["$referrerDomain"], want[a.referrer])
		}
	}
}

func TestAIFeaturedPagesExist(t *testing.T) {
	for _, s := range aiFeaturedPages {
		switch s.kind {
		case "product_viewed":
			if _, ok := productBySlug[s.arg]; !ok {
				t.Errorf("no product %q", s.arg)
			}
		case "help_article_viewed":
			if !slices.ContainsFunc(helpArticles, func(a struct{ id, title, category string }) bool { return a.id == s.arg }) {
				t.Errorf("no help article %q", s.arg)
			}
		default:
			t.Errorf("featured page step %q", s.kind)
		}
	}
}

func TestBotSessionsCarryIngestCrawlerTags(t *testing.T) {
	for _, bp := range botProfiles {
		if agent, ok := botCrawler(bp); ok && slices.Contains([]string{"OpenAI", "Anthropic", "Perplexity"}, agent.Company) {
			t.Errorf("%s sends page views, but AI crawlers never run JavaScript", agent.Name)
		}
	}
	f := newSessionFactory()
	now := time.Now()
	for range 300 {
		for _, e := range f.botSession(now, now.Add(time.Hour)) {
			category, _ := e.autoProperties["$crawler_category"].(string)
			company, _ := e.autoProperties["$crawler_company"].(string)
			var want [2]string
			switch e.autoProperties["$bot_reason"] {
			case "Googlebot":
				want = [2]string{"search", "Google"}
			case "bingbot":
				want = [2]string{"search", "Microsoft"}
			case "AhrefsBot", "SemrushBot":
				want = [2]string{"seo", ""}
			}
			if [2]string{category, company} != want {
				t.Fatalf("%v page view tagged %q/%q, want %q/%q", e.autoProperties["$bot_reason"], category, company, want[0], want[1])
			}
		}
	}
}

func TestLiveCrawlerRequest(t *testing.T) {
	now := time.Now()
	agents := map[string]bool{}
	for range 500 {
		e := LiveCrawlerRequest("proj", now)
		name, _ := e.AutoProperties["$bot_reason"].(string)
		if e.EventID == "" || e.Kind != crawler.EventKind || !e.OccurTime.Equal(now) || e.SessionID != crawler.SessionID("proj", name, now) {
			t.Fatalf("live request = %+v", e)
		}
		agents[name] = true
	}
	if len(agents) < 10 {
		t.Errorf("500 live requests came from %d crawlers", len(agents))
	}
	if rate := CrawlerRequestsPerMinute(now); rate < 1 || rate > 20 {
		t.Errorf("%.1f crawler requests a minute", rate)
	}
}

func TestCrawlerBackfillOnClickHouse(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ch := testutil.SetupClickHouse(t)
	ctx := t.Context()
	now := time.Now().UTC()
	from := now.AddDate(0, 0, -2)
	store := chStore{ch: ch.Conn}

	if _, ok, err := store.earliestCrawlerRequest(ctx, "proj"); err != nil || ok {
		t.Fatalf("empty project: found %v, err %v", ok, err)
	}
	if _, err := backfillCrawlerRequests(ctx, &failingStore{crawlerStore: store, failAfter: 1}, "proj", time.Time{}, from, now); !errors.Is(err, errStoreDown) {
		t.Fatalf("cut run: err = %v, want the store's error", err)
	}
	if _, err := backfillCrawlerRequests(ctx, store, "proj", time.Time{}, from, now); err != nil {
		t.Fatalf("resumed run: %v", err)
	}

	whole := backfillInMemory(t, from, now)
	var rows, ids, tagged, statuses uint64
	if err := ch.Conn.QueryRow(ctx, `
		SELECT count(), uniqExact(event_id),
		       countIf(bot AND bot_reason != '' AND pathname != '' AND hostname = 'pugandpals.example.com'),
		       countIf(variantElement(auto_properties['$status_code'], 'Int64') >= 200)
		FROM events WHERE project_id = ? AND kind = ?`,
		"proj", crawler.EventKind,
	).Scan(&rows, &ids, &tagged, &statuses); err != nil {
		t.Fatal(err)
	}
	if want := uint64(len(whole.events)); rows != want || ids != want || tagged != want || statuses != want {
		t.Errorf("stored %d rows, %d ids, %d tagged, %d statuses; want %d of each", rows, ids, tagged, statuses, want)
	}
	if n, err := EventCount(ctx, ch.Conn, "proj"); err != nil || n != 0 {
		t.Errorf("EventCount over crawler requests alone = %d (err %v), want 0", n, err)
	}

	got, ok, err := store.earliestCrawlerRequest(ctx, "proj")
	want, _, _ := whole.earliestCrawlerRequest(ctx, "proj")
	if err != nil || !ok || got.Sub(want).Abs() >= time.Millisecond {
		t.Errorf("earliest = %v (found %v, err %v), want %v", got, ok, err, want)
	}
}
