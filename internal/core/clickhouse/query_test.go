package clickhouse_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	chq "github.com/pug-sh/pug/internal/core/clickhouse"
)

// build calls Build on q and fatals on error.
func build(t *testing.T, q *chq.Query) (string, []any) {
	t.Helper()
	sql, args, err := q.Build()
	if err != nil {
		t.Fatalf("Build() error: %v", err)
	}
	return sql, args
}

// --- Condition constructors ---

func TestConditionConstructors(t *testing.T) {
	t.Run("Eq", func(t *testing.T) {
		c := chq.Eq("kind", "page_view")
		sql, args := build(t, chq.NewQuery().Select("1").From("events").Where(c))
		if want := "SELECT 1\nFROM events\nWHERE kind = ?"; sql != want {
			t.Errorf("sql:\ngot  %q\nwant %q", sql, want)
		}
		if diff := cmp.Diff([]any{"page_view"}, args); diff != "" {
			t.Errorf("args mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("Neq", func(t *testing.T) {
		sql, args := build(t, chq.NewQuery().Select("1").From("e").Where(chq.Neq("kind", "x")))
		if want := "SELECT 1\nFROM e\nWHERE kind != ?"; sql != want {
			t.Errorf("sql: got %q want %q", sql, want)
		}
		if diff := cmp.Diff([]any{"x"}, args); diff != "" {
			t.Errorf("args: %s", diff)
		}
	})

	t.Run("Gte", func(t *testing.T) {
		ts := time.Unix(0, 0)
		sql, args := build(t, chq.NewQuery().Select("1").From("e").Where(chq.Gte("occur_time", ts)))
		if want := "SELECT 1\nFROM e\nWHERE occur_time >= ?"; sql != want {
			t.Errorf("sql: got %q want %q", sql, want)
		}
		if diff := cmp.Diff([]any{ts}, args); diff != "" {
			t.Errorf("args: %s", diff)
		}
	})

	t.Run("Gt", func(t *testing.T) {
		sql, _ := build(t, chq.NewQuery().Select("1").From("e").Where(chq.Gt("x", 1)))
		if want := "SELECT 1\nFROM e\nWHERE x > ?"; sql != want {
			t.Errorf("sql: got %q want %q", sql, want)
		}
	})

	t.Run("Lte", func(t *testing.T) {
		sql, _ := build(t, chq.NewQuery().Select("1").From("e").Where(chq.Lte("x", 5)))
		if want := "SELECT 1\nFROM e\nWHERE x <= ?"; sql != want {
			t.Errorf("sql: got %q want %q", sql, want)
		}
	})

	t.Run("Lt", func(t *testing.T) {
		sql, _ := build(t, chq.NewQuery().Select("1").From("e").Where(chq.Lt("occur_time", 100)))
		if want := "SELECT 1\nFROM e\nWHERE occur_time < ?"; sql != want {
			t.Errorf("sql: got %q want %q", sql, want)
		}
	})

	t.Run("RawCond", func(t *testing.T) {
		sql, args := build(t, chq.NewQuery().Select("1").From("e").Where(chq.RawCond("kind IN (?, ?)", "a", "b")))
		if want := "SELECT 1\nFROM e\nWHERE kind IN (?, ?)"; sql != want {
			t.Errorf("sql: got %q want %q", sql, want)
		}
		if diff := cmp.Diff([]any{"a", "b"}, args); diff != "" {
			t.Errorf("args: %s", diff)
		}
	})
}

// --- And / Or ---

func TestAndOr(t *testing.T) {
	t.Run("And_two_wraps_parens", func(t *testing.T) {
		c := chq.And(chq.Eq("a", 1), chq.Eq("b", 2))
		sql, args := build(t, chq.NewQuery().Select("1").From("e").Where(c))
		if want := "SELECT 1\nFROM e\nWHERE (a = ? AND b = ?)"; sql != want {
			t.Errorf("sql: got %q want %q", sql, want)
		}
		if diff := cmp.Diff([]any{1, 2}, args); diff != "" {
			t.Errorf("args: %s", diff)
		}
	})

	t.Run("And_one_passthrough", func(t *testing.T) {
		c := chq.And(chq.Eq("a", 1))
		sql, args := build(t, chq.NewQuery().Select("1").From("e").Where(c))
		if want := "SELECT 1\nFROM e\nWHERE a = ?"; sql != want {
			t.Errorf("sql: got %q want %q", sql, want)
		}
		if diff := cmp.Diff([]any{1}, args); diff != "" {
			t.Errorf("args: %s", diff)
		}
	})

	t.Run("And_zero_skipped", func(t *testing.T) {
		c := chq.And()
		sql, args := build(t, chq.NewQuery().Select("1").From("e").Where(c))
		if want := "SELECT 1\nFROM e"; sql != want {
			t.Errorf("sql: got %q want %q", sql, want)
		}
		if len(args) != 0 {
			t.Errorf("expected no args, got %v", args)
		}
	})

	t.Run("Or_two_wraps_parens", func(t *testing.T) {
		c := chq.Or(chq.Eq("kind", "a"), chq.Eq("kind", "b"))
		sql, args := build(t, chq.NewQuery().Select("1").From("e").Where(c))
		if want := "SELECT 1\nFROM e\nWHERE (kind = ? OR kind = ?)"; sql != want {
			t.Errorf("sql: got %q want %q", sql, want)
		}
		if diff := cmp.Diff([]any{"a", "b"}, args); diff != "" {
			t.Errorf("args: %s", diff)
		}
	})

	t.Run("Or_skips_zeros", func(t *testing.T) {
		c := chq.Or(chq.Condition{}, chq.Eq("kind", "x"), chq.Condition{})
		sql, _ := build(t, chq.NewQuery().Select("1").From("e").Where(c))
		if want := "SELECT 1\nFROM e\nWHERE kind = ?"; sql != want {
			t.Errorf("sql: got %q want %q", sql, want)
		}
	})
}

// --- When ---

func TestWhen(t *testing.T) {
	t.Run("true returns condition", func(t *testing.T) {
		c := chq.When(true, chq.Eq("kind", "x"))
		sql, _ := build(t, chq.NewQuery().Select("1").From("e").Where(c))
		if want := "SELECT 1\nFROM e\nWHERE kind = ?"; sql != want {
			t.Errorf("sql: got %q want %q", sql, want)
		}
	})

	t.Run("false returns zero", func(t *testing.T) {
		c := chq.When(false, chq.Eq("kind", "x"))
		sql, _ := build(t, chq.NewQuery().Select("1").From("e").Where(c))
		if want := "SELECT 1\nFROM e"; sql != want {
			t.Errorf("sql: got %q want %q", sql, want)
		}
	})
}

// --- Query.Build ---

func TestQueryBuild(t *testing.T) {
	t.Run("missing SELECT", func(t *testing.T) {
		if _, _, err := chq.NewQuery().From("events").Build(); err == nil {
			t.Fatal("expected error for missing SELECT")
		}
	})

	t.Run("missing FROM", func(t *testing.T) {
		if _, _, err := chq.NewQuery().Select("1").Build(); err == nil {
			t.Fatal("expected error for missing FROM")
		}
	})

	t.Run("multiple WHERE calls AND-joined", func(t *testing.T) {
		sql, args := build(t,
			chq.NewQuery().
				Select("count(*)").
				From("events").
				Where(chq.Eq("project_id", "proj-1")).
				Where(chq.Eq("kind", "click")),
		)
		want := "SELECT count(*)\nFROM events\nWHERE project_id = ?\nAND kind = ?"
		if sql != want {
			t.Errorf("sql:\ngot  %q\nwant %q", sql, want)
		}
		if diff := cmp.Diff([]any{"proj-1", "click"}, args); diff != "" {
			t.Errorf("args: %s", diff)
		}
	})

	t.Run("GroupBy and OrderBy", func(t *testing.T) {
		sql, _ := build(t,
			chq.NewQuery().
				Select("toStartOfDay(occur_time) AS t", "count(*) AS value").
				From("events").
				Where(chq.Eq("project_id", "p")).
				GroupBy("t").
				OrderBy("t ASC"),
		)
		want := "SELECT toStartOfDay(occur_time) AS t,\ncount(*) AS value\nFROM events\nWHERE project_id = ?\nGROUP BY t\nORDER BY t ASC"
		if sql != want {
			t.Errorf("sql:\ngot  %q\nwant %q", sql, want)
		}
	})

	t.Run("Limit appends arg", func(t *testing.T) {
		sql, args := build(t,
			chq.NewQuery().
				Select("distinct_id").
				From("events").
				Where(chq.Eq("project_id", "p")).
				OrderBy("distinct_id ASC").
				Limit(50),
		)
		want := "SELECT distinct_id\nFROM events\nWHERE project_id = ?\nORDER BY distinct_id ASC\nLIMIT ?"
		if sql != want {
			t.Errorf("sql:\ngot  %q\nwant %q", sql, want)
		}
		if diff := cmp.Diff([]any{"p", int64(50)}, args); diff != "" {
			t.Errorf("args: %s", diff)
		}
	})

	t.Run("no WHERE clause omitted", func(t *testing.T) {
		sql, args := build(t, chq.NewQuery().Select("1").From("events"))
		if want := "SELECT 1\nFROM events"; sql != want {
			t.Errorf("sql: got %q want %q", sql, want)
		}
		if len(args) != 0 {
			t.Errorf("expected no args, got %v", args)
		}
	})
}

// --- SelectExpr ---

func TestSelectExpr(t *testing.T) {
	t.Run("args emitted between CTE and WHERE", func(t *testing.T) {
		sql, args := build(t,
			chq.NewQuery().
				Select("distinct_id").
				SelectExpr("windowFunnel(600)(occur_time, kind = ?, kind = ?) AS level", "signup", "purchase").
				From("events").
				Where(chq.Eq("project_id", "p1")).
				GroupBy("distinct_id"),
		)
		// SELECT args (signup, purchase) must come before WHERE args (p1)
		wantArgs := []any{"signup", "purchase", "p1"}
		if diff := cmp.Diff(wantArgs, args); diff != "" {
			t.Errorf("args order mismatch (-want +got):\n%s", diff)
		}
		if !containsStr(sql, "windowFunnel(600)") {
			t.Errorf("expected windowFunnel in SQL, got: %s", sql)
		}
	})

	t.Run("mixed with CTE args", func(t *testing.T) {
		inner := chq.NewQuery().
			Select("distinct_id").
			SelectExpr("windowFunnel(300)(occur_time, kind = ?) AS level", "click").
			From("events").
			Where(chq.Eq("project_id", "inner-p")).
			GroupBy("distinct_id")

		_, args := build(t,
			chq.NewQuery().
				With("funnel", inner).
				Select("level", "count() AS cnt").
				From("funnel").
				Where(chq.Gt("level", 0)).
				GroupBy("level"),
		)
		// CTE: selectArgs(click) + WHERE(inner-p), then main: WHERE(0)
		wantArgs := []any{"click", "inner-p", 0}
		if diff := cmp.Diff(wantArgs, args); diff != "" {
			t.Errorf("args order mismatch (-want +got):\n%s", diff)
		}
	})
}

// --- CTE ---

func TestCTE(t *testing.T) {
	t.Run("CTE args emitted before main args", func(t *testing.T) {
		cteQ := chq.NewQuery().
			Select("auto_properties['$country'] AS breakdown_0").
			From("events").
			Where(chq.Eq("project_id", "cte-proj"), chq.Eq("kind", "purchase")).
			GroupBy("breakdown_0").
			OrderBy("count(*) DESC").
			Limit(10)

		mainQ := chq.NewQuery().
			Select("toStartOfDay(occur_time) AS t", "breakdown_0", "toFloat64(count(*)) AS value").
			From("events").
			Where(chq.Eq("project_id", "main-proj"), chq.Eq("kind", "purchase")).
			GroupBy("t", "breakdown_0").
			OrderBy("t ASC").
			With("top_vals", cteQ)

		sql, args, err := mainQ.Build()
		if err != nil {
			t.Fatalf("Build() error: %v", err)
		}

		// CTE args come first (cte-proj, purchase, 10), then main args (main-proj, purchase).
		wantArgs := []any{"cte-proj", "purchase", int64(10), "main-proj", "purchase"}
		if diff := cmp.Diff(wantArgs, args); diff != "" {
			t.Errorf("args order mismatch (-want +got):\n%s", diff)
		}

		// SQL must start with WITH
		if len(sql) < 4 || sql[:4] != "WITH" {
			t.Errorf("expected SQL to start with WITH, got: %q", sql[:clamp(len(sql), 30)])
		}
	})

	t.Run("CTE sub-query error propagates", func(t *testing.T) {
		badCTE := chq.NewQuery().From("events") // missing SELECT
		if _, _, err := chq.NewQuery().
			Select("1").
			From("events").
			With("bad", badCTE).
			Build(); err == nil {
			t.Fatal("expected error from bad CTE sub-query")
		}
	})
}

// --- UnionAll ---

func TestUnionAll(t *testing.T) {
	q1 := chq.NewQuery().
		Select("toStartOfDay(occur_time) AS t", "kind AS event_kind", "toFloat64(count(*)) AS value").
		From("events").
		Where(chq.Eq("project_id", "p"), chq.Eq("kind", "purchase")).
		GroupBy("t", "event_kind")

	q2 := chq.NewQuery().
		Select("toStartOfDay(occur_time) AS t", "kind AS event_kind", "toFloat64(count(DISTINCT distinct_id)) AS value").
		From("events").
		Where(chq.Eq("project_id", "p"), chq.Eq("kind", "page_view")).
		GroupBy("t", "event_kind")

	t.Run("args are q1 then q2", func(t *testing.T) {
		_, args, err := chq.UnionAll(q1, q2).OrderBy("t ASC", "event_kind ASC").Build()
		if err != nil {
			t.Fatalf("Build() error: %v", err)
		}
		wantArgs := []any{"p", "purchase", "p", "page_view"}
		if diff := cmp.Diff(wantArgs, args); diff != "" {
			t.Errorf("args order mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("SQL contains UNION ALL", func(t *testing.T) {
		sql, _, err := chq.UnionAll(q1, q2).Build()
		if err != nil {
			t.Fatalf("Build() error: %v", err)
		}
		if !containsStr(sql, "UNION ALL") {
			t.Errorf("expected UNION ALL in SQL, got: %s", sql)
		}
	})

	t.Run("OrderBy appended after union", func(t *testing.T) {
		sql, _, err := chq.UnionAll(q1, q2).OrderBy("t ASC").Build()
		if err != nil {
			t.Fatalf("Build() error: %v", err)
		}
		if !containsStr(sql, "ORDER BY t ASC") {
			t.Errorf("expected ORDER BY t ASC in SQL, got: %s", sql)
		}
	})

	t.Run("empty queries error", func(t *testing.T) {
		if _, _, err := chq.UnionAll().Build(); err == nil {
			t.Fatal("expected error for empty UnionAll")
		}
	})

	t.Run("sub-query error propagates", func(t *testing.T) {
		bad := chq.NewQuery().From("events") // missing SELECT
		if _, _, err := chq.UnionAll(q1, bad).Build(); err == nil {
			t.Fatal("expected error from bad sub-query")
		}
	})
}

// --- SETTINGS helpers ---

func TestSettingsHelpers(t *testing.T) {
	t.Run("WithQueryCache appends settings after SELECT/FROM", func(t *testing.T) {
		sql, args := build(t,
			chq.NewQuery().
				Select("count(*)").
				From("events").
				Where(chq.Eq("project_id", "p")).
				WithQueryCache(60),
		)
		want := "SELECT count(*)\nFROM events\nWHERE project_id = ?\nSETTINGS use_query_cache = 1, query_cache_ttl = 60"
		if sql != want {
			t.Errorf("sql:\ngot  %q\nwant %q", sql, want)
		}
		if diff := cmp.Diff([]any{"p"}, args); diff != "" {
			t.Errorf("args: %s", diff)
		}
	})

	t.Run("single call emits both settings comma-separated", func(t *testing.T) {
		sql, _ := build(t,
			chq.NewQuery().
				Select("1").
				From("events").
				WithQueryCache(60),
		)
		if !containsStr(sql, "SETTINGS use_query_cache = 1, query_cache_ttl = 60") {
			t.Errorf("expected SETTINGS clause, got: %s", sql)
		}
	})

	t.Run("repeated WithQueryCache calls dedup by key — last TTL wins", func(t *testing.T) {
		sql, _ := build(t,
			chq.NewQuery().
				Select("1").
				From("events").
				WithQueryCache(60).
				WithQueryCache(120),
		)
		want := "SETTINGS use_query_cache = 1, query_cache_ttl = 120"
		if !strings.HasSuffix(sql, want) {
			t.Errorf("expected SETTINGS to end with %q, got: %s", want, sql)
		}
		// Ensure the superseded value is gone (no duplicate emission).
		if containsStr(sql, "query_cache_ttl = 60") {
			t.Errorf("expected superseded TTL to be removed, got: %s", sql)
		}
	})

	t.Run("Build is idempotent under repeated calls", func(t *testing.T) {
		q := chq.NewQuery().
			Select("1").
			From("events").
			Where(chq.Eq("project_id", "p")).
			WithQueryCache(60)
		sql1, args1, err := q.Build()
		if err != nil {
			t.Fatalf("first Build() error: %v", err)
		}
		sql2, args2, err := q.Build()
		if err != nil {
			t.Fatalf("second Build() error: %v", err)
		}
		if sql1 != sql2 {
			t.Errorf("Build() not idempotent:\nfirst : %q\nsecond: %q", sql1, sql2)
		}
		if diff := cmp.Diff(args1, args2); diff != "" {
			t.Errorf("args drifted between Build() calls: %s", diff)
		}
	})

	t.Run("SETTINGS placement holds with Limit(0)", func(t *testing.T) {
		sql, args := build(t,
			chq.NewQuery().
				Select("1").
				From("events").
				Limit(0).
				WithQueryCache(60),
		)
		if !strings.HasSuffix(sql, "SETTINGS use_query_cache = 1, query_cache_ttl = 60") {
			t.Errorf("expected SETTINGS at end, got: %s", sql)
		}
		if !containsStr(sql, "LIMIT ?") {
			t.Errorf("expected LIMIT clause, got: %s", sql)
		}
		if diff := cmp.Diff([]any{int64(0)}, args); diff != "" {
			t.Errorf("args: %s", diff)
		}
	})

	t.Run("WithQueryCache panics on zero TTL", func(t *testing.T) {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("expected panic on zero TTL")
			}
			msg, ok := r.(string)
			if !ok {
				t.Fatalf("expected panic value to be string, got %T: %v", r, r)
			}
			if !containsStr(msg, "Query.WithQueryCache") {
				t.Errorf("expected panic message to mention Query.WithQueryCache, got: %s", msg)
			}
			if !containsStr(msg, "ttlSeconds must be positive") {
				t.Errorf("expected panic message to mention ttlSeconds, got: %s", msg)
			}
		}()
		chq.NewQuery().WithQueryCache(0)
	})

	t.Run("WithQueryCache panics on negative TTL", func(t *testing.T) {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("expected panic on negative TTL")
			}
			msg, ok := r.(string)
			if !ok {
				t.Fatalf("expected panic value to be string, got %T: %v", r, r)
			}
			if !containsStr(msg, "got -1") {
				t.Errorf("expected panic message to include offending value, got: %s", msg)
			}
		}()
		chq.NewQuery().WithQueryCache(-1)
	})

	t.Run("SETTINGS is the last clause after LIMIT", func(t *testing.T) {
		sql, _ := build(t,
			chq.NewQuery().
				Select("1").
				From("events").
				Limit(10).
				WithQueryCache(60),
		)
		if !strings.HasSuffix(sql, "SETTINGS use_query_cache = 1, query_cache_ttl = 60") {
			t.Errorf("expected SETTINGS clause to be the last line of SQL, got: %s", sql)
		}
	})

	t.Run("no helper calls omits SETTINGS clause", func(t *testing.T) {
		sql, _ := build(t, chq.NewQuery().Select("1").From("events"))
		if containsStr(sql, "SETTINGS") {
			t.Errorf("expected no SETTINGS clause, got: %s", sql)
		}
	})
}

func TestUnionAllSettingsHelpers(t *testing.T) {
	q1 := chq.NewQuery().Select("1 AS x").From("events").Where(chq.Eq("project_id", "p"))
	q2 := chq.NewQuery().Select("2 AS x").From("events").Where(chq.Eq("project_id", "p"))

	t.Run("WithQueryCache appends settings after ORDER BY", func(t *testing.T) {
		sql, _, err := chq.UnionAll(q1, q2).
			OrderBy("x ASC").
			WithQueryCache(60).
			Build()
		if err != nil {
			t.Fatalf("Build() error: %v", err)
		}
		if !containsStr(sql, "ORDER BY x ASC") || !containsStr(sql, "SETTINGS use_query_cache = 1, query_cache_ttl = 60") {
			t.Errorf("expected ORDER BY and SETTINGS, got: %s", sql)
		}
		orderIdx := indexStr(sql, "ORDER BY")
		settingsIdx := indexStr(sql, "SETTINGS")
		if settingsIdx < orderIdx {
			t.Errorf("expected SETTINGS after ORDER BY, got: %s", sql)
		}
	})

	t.Run("multiple helper calls are comma-separated", func(t *testing.T) {
		sql, _, err := chq.UnionAll(q1, q2).
			WithQueryCache(60).
			Build()
		if err != nil {
			t.Fatalf("Build() error: %v", err)
		}
		if !containsStr(sql, "SETTINGS use_query_cache = 1, query_cache_ttl = 60") {
			t.Errorf("expected SETTINGS clause, got: %s", sql)
		}
	})

	t.Run("no settings omits SETTINGS clause", func(t *testing.T) {
		sql, _, err := chq.UnionAll(q1, q2).Build()
		if err != nil {
			t.Fatalf("Build() error: %v", err)
		}
		if containsStr(sql, "SETTINGS") {
			t.Errorf("expected no SETTINGS clause, got: %s", sql)
		}
	})

	t.Run("UnionQuery WithQueryCache panics on zero TTL", func(t *testing.T) {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("expected panic on zero TTL")
			}
			msg, ok := r.(string)
			if !ok {
				t.Fatalf("expected panic value to be string, got %T: %v", r, r)
			}
			if !containsStr(msg, "UnionQuery.WithQueryCache") {
				t.Errorf("expected panic message to mention UnionQuery.WithQueryCache, got: %s", msg)
			}
		}()
		chq.UnionAll(q1, q2).WithQueryCache(0)
	})

	t.Run("UnionQuery WithQueryCache panics on negative TTL", func(t *testing.T) {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("expected panic on negative TTL")
			}
			msg, ok := r.(string)
			if !ok {
				t.Fatalf("expected panic value to be string, got %T: %v", r, r)
			}
			if !containsStr(msg, "got -1") {
				t.Errorf("expected panic message to include offending value, got: %s", msg)
			}
		}()
		chq.UnionAll(q1, q2).WithQueryCache(-1)
	})

	t.Run("repeated WithQueryCache calls dedup by key — last TTL wins", func(t *testing.T) {
		sql, _, err := chq.UnionAll(q1, q2).
			WithQueryCache(60).
			WithQueryCache(120).
			Build()
		if err != nil {
			t.Fatalf("Build() error: %v", err)
		}
		want := "SETTINGS use_query_cache = 1, query_cache_ttl = 120"
		if !strings.HasSuffix(sql, want) {
			t.Errorf("expected SETTINGS to end with %q, got: %s", want, sql)
		}
		if containsStr(sql, "query_cache_ttl = 60") {
			t.Errorf("expected superseded TTL to be removed, got: %s", sql)
		}
	})
}

// TestSettingsBuildGuards verifies that SETTINGS on inner queries panics at Build() time.
// ClickHouse only accepts SETTINGS at the top level of a SELECT/UNION; emitting SETTINGS
// inside a CTE body or a UNION branch would produce SQL the server rejects. Since
// `analyticsCacheTTL` is package-private and only applied to outer queries, this guard
// is unreachable except via a programmer error — panic surfaces it on the first run.
func TestSettingsBuildGuards(t *testing.T) {
	expectPanic := func(t *testing.T, want string, fn func()) {
		t.Helper()
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("expected panic, got nil")
			}
			msg, ok := r.(string)
			if !ok {
				t.Fatalf("expected panic value to be string, got %T: %v", r, r)
			}
			if !containsStr(msg, want) {
				t.Errorf("expected panic to contain %q, got: %s", want, msg)
			}
		}()
		fn()
	}

	t.Run("Query.Build panics when CTE sub-query has SETTINGS", func(t *testing.T) {
		expectPanic(t, `cte "top_vals"`, func() {
			inner := chq.NewQuery().Select("1").From("events").WithQueryCache(60)
			outer := chq.NewQuery().Select("*").From("cte").With("top_vals", inner)
			_, _, _ = outer.Build()
		})
	})

	t.Run("Query.Build panics when nested CTE (depth 2) has SETTINGS", func(t *testing.T) {
		expectPanic(t, `cte "inner_cte"`, func() {
			deepest := chq.NewQuery().Select("1").From("events").WithQueryCache(60)
			middle := chq.NewQuery().Select("*").From("base").With("inner_cte", deepest)
			outer := chq.NewQuery().Select("*").From("middle").With("middle_cte", middle)
			_, _, _ = outer.Build()
		})
	})

	t.Run("UnionQuery.Build panics when first branch has SETTINGS", func(t *testing.T) {
		expectPanic(t, "query 0", func() {
			q1 := chq.NewQuery().Select("1 AS x").From("events").WithQueryCache(60)
			q2 := chq.NewQuery().Select("2 AS x").From("events")
			_, _, _ = chq.UnionAll(q1, q2).Build()
		})
	})

	t.Run("UnionQuery.Build panics when later branch has SETTINGS", func(t *testing.T) {
		expectPanic(t, "query 1", func() {
			q1 := chq.NewQuery().Select("1 AS x").From("events")
			q2 := chq.NewQuery().Select("2 AS x").From("events").WithQueryCache(60)
			_, _, _ = chq.UnionAll(q1, q2).Build()
		})
	})
}

// --- Complex integration-style examples ---

func TestComplexQueries(t *testing.T) {
	t.Run("simple trends query", func(t *testing.T) {
		from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
		to := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)

		sql, args, err := chq.NewQuery().
			Select("toStartOfDay(occur_time) AS t", "toFloat64(count(*)) AS value").
			From("events").
			Where(
				chq.Eq("project_id", "proj-abc"),
				chq.Gte("occur_time", from),
				chq.Lt("occur_time", to),
				chq.Eq("kind", "page_view"),
			).
			GroupBy("t").
			OrderBy("t ASC").
			Build()
		if err != nil {
			t.Fatalf("Build() error: %v", err)
		}

		wantSQL := "SELECT toStartOfDay(occur_time) AS t,\ntoFloat64(count(*)) AS value\n" +
			"FROM events\n" +
			"WHERE project_id = ?\n" +
			"AND occur_time >= ?\n" +
			"AND occur_time < ?\n" +
			"AND kind = ?\n" +
			"GROUP BY t\n" +
			"ORDER BY t ASC"
		if sql != wantSQL {
			t.Errorf("sql:\ngot  %q\nwant %q", sql, wantSQL)
		}
		if diff := cmp.Diff([]any{"proj-abc", from, to, "page_view"}, args); diff != "" {
			t.Errorf("args: %s", diff)
		}
	})

	t.Run("segment users with cursor and optional kind", func(t *testing.T) {
		from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
		to := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
		kind := "purchase"
		pageToken := "user-123"
		var pageSize int64 = 100

		sql, args, err := chq.NewQuery().
			Select("DISTINCT distinct_id").
			From("events").
			Where(
				chq.Eq("project_id", "proj-x"),
				chq.Gte("occur_time", from),
				chq.Lt("occur_time", to),
				chq.When(kind != "", chq.Eq("kind", kind)),
				chq.When(pageToken != "", chq.Gt("distinct_id", pageToken)),
			).
			OrderBy("distinct_id ASC").
			Limit(pageSize).
			Build()
		if err != nil {
			t.Fatalf("Build() error: %v", err)
		}

		wantArgs := []any{"proj-x", from, to, "purchase", "user-123", pageSize}
		if diff := cmp.Diff(wantArgs, args); diff != "" {
			t.Errorf("args: %s", diff)
		}
		if !containsStr(sql, "LIMIT ?") {
			t.Errorf("expected LIMIT ? in sql: %s", sql)
		}
	})

	t.Run("OR-joined multi-event filter", func(t *testing.T) {
		evCond := chq.Or(
			chq.And(chq.Eq("kind", "purchase"), chq.Gte("price", 50)),
			chq.Eq("kind", "page_view"),
		)

		sql, args, err := chq.NewQuery().
			Select("DISTINCT distinct_id").
			From("events").
			Where(chq.Eq("project_id", "p")).
			Where(evCond).
			Build()
		if err != nil {
			t.Fatalf("Build() error: %v", err)
		}

		if !containsStr(sql, "OR") {
			t.Errorf("expected OR in sql: %s", sql)
		}
		// project_id, kind=purchase, price=50, kind=page_view
		if diff := cmp.Diff([]any{"p", "purchase", 50, "page_view"}, args); diff != "" {
			t.Errorf("args: %s", diff)
		}
	})
}

func containsStr(s, sub string) bool {
	return indexStr(s, sub) >= 0
}

func indexStr(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func clamp(n, max int) int {
	if n < max {
		return n
	}
	return max
}
