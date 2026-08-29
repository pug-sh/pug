package clickhouse

import (
	"testing"

	"github.com/pug-sh/pug/internal/autoprop"
)

// botColumn restates a name promotedAutoColumns owns; nothing links the two.
func TestBotColumnMatchesPromotedMapping(t *testing.T) {
	if got := promotedAutoByProperty[autoprop.PropBot].Column; got != botColumn {
		t.Errorf("botColumn = %q, promoted mapping says %q", botColumn, got)
	}
}

func TestBotFilter(t *testing.T) {
	if c := BotFilter(true, ""); !c.IsZero() {
		t.Errorf("inclusion must be the zero condition (skipped by Where), got %q", c.SQL())
	}
	if c := BotFilter(false, ""); c.SQL() != "bot = 0" || len(c.Args()) != 0 {
		t.Errorf("unaliased cond = %q args=%v, want %q with no args", c.SQL(), c.Args(), "bot = 0")
	}
	if c := BotFilter(false, "e"); c.SQL() != "e.bot = 0" {
		t.Errorf("aliased cond = %q, want %q", c.SQL(), "e.bot = 0")
	}
}
