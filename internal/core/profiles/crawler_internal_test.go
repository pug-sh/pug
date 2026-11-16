package profiles

import (
	"testing"

	"github.com/pug-sh/pug/internal/crawler"
)

func TestCrawlerIDPrefixMatchesTheCrawlerPackage(t *testing.T) {
	if crawlerIDPrefix != crawler.DistinctIDPrefix {
		t.Errorf("the person list skips %q, but crawler ids start %q", crawlerIDPrefix, crawler.DistinctIDPrefix)
	}
}
