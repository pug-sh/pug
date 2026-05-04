package clickhouse

import (
	"fmt"
	"strconv"
	"strings"
)

// Condition is an opaque SQL fragment with its positional args.
type Condition struct {
	sql  string
	args []any
}

// SQL returns the condition SQL fragment.
func (c Condition) SQL() string {
	return c.sql
}

// Args returns positional args for this condition.
func (c Condition) Args() []any {
	return c.args
}

// IsZero reports whether the condition is a zero-value (should be skipped).
func (c Condition) IsZero() bool {
	return c.sql == ""
}

// Eq returns a condition: col = ?
func Eq(col string, val any) Condition {
	return Condition{sql: col + " = ?", args: []any{val}}
}

// Neq returns a condition: col != ?
func Neq(col string, val any) Condition {
	return Condition{sql: col + " != ?", args: []any{val}}
}

// Gte returns a condition: col >= ?
func Gte(col string, val any) Condition {
	return Condition{sql: col + " >= ?", args: []any{val}}
}

// Gt returns a condition: col > ?
func Gt(col string, val any) Condition {
	return Condition{sql: col + " > ?", args: []any{val}}
}

// Lte returns a condition: col <= ?
func Lte(col string, val any) Condition {
	return Condition{sql: col + " <= ?", args: []any{val}}
}

// Lt returns a condition: col < ?
func Lt(col string, val any) Condition {
	return Condition{sql: col + " < ?", args: []any{val}}
}

// RawCond wraps a raw SQL clause and its args into a Condition.
func RawCond(sql string, args ...any) Condition {
	return Condition{sql: sql, args: args}
}

// And joins conditions with AND. Zero-value conditions are skipped.
// Returns zero-value if no non-zero conditions remain.
// Returns the single condition unchanged if only one remains.
// Wraps in parens when two or more conditions remain.
func And(conds ...Condition) Condition {
	active := filterZero(conds)
	switch len(active) {
	case 0:
		return Condition{}
	case 1:
		return active[0]
	}
	return join(" AND ", active)
}

// Or joins conditions with OR. Zero-value conditions are skipped.
// Returns zero-value if no non-zero conditions remain.
// Returns the single condition unchanged if only one remains.
// Wraps in parens when two or more conditions remain.
func Or(conds ...Condition) Condition {
	active := filterZero(conds)
	switch len(active) {
	case 0:
		return Condition{}
	case 1:
		return active[0]
	}
	return join(" OR ", active)
}

// When returns c if ok is true, otherwise a zero-value Condition.
func When(ok bool, c Condition) Condition {
	if ok {
		return c
	}
	return Condition{}
}

func filterZero(conds []Condition) []Condition {
	out := conds[:0:len(conds)]
	for _, c := range conds {
		if !c.IsZero() {
			out = append(out, c)
		}
	}
	return out
}

func join(op string, conds []Condition) Condition {
	parts := make([]string, len(conds))
	var args []any
	for i, c := range conds {
		parts[i] = c.sql
		args = append(args, c.args...)
	}
	return Condition{
		sql:  "(" + strings.Join(parts, op) + ")",
		args: args,
	}
}

// cte holds a named CTE sub-query.
type cte struct {
	name string
	sub  *Query
}

// settingKV holds one key/value pair emitted in a SETTINGS clause.
// Build() formats each pair as "key = value"; producers (intSetting, etc.)
// are responsible for choosing a value form ClickHouse will accept.
type settingKV struct {
	key   string
	value string
}

func (s settingKV) format() string {
	return s.key + " = " + s.value
}

// intSetting returns a settingKV for an integer-valued ClickHouse setting.
func intSetting(key string, value int) settingKV {
	return settingKV{key: key, value: strconv.Itoa(value)}
}

// upsertSetting replaces the value for an existing key, or appends if the key is new.
// Keeps the SETTINGS clause idempotent across repeated builder calls.
func upsertSetting(settings []settingKV, s settingKV) []settingKV {
	for i := range settings {
		if settings[i].key == s.key {
			settings[i].value = s.value
			return settings
		}
	}
	return append(settings, s)
}

// Query builds a single SELECT statement.
type Query struct {
	selects    []string
	selectArgs []any // positional args for SELECT expressions (e.g., windowFunnel conditions)
	from       string
	wheres     []Condition
	groupBy    []string
	orderBy    []string
	limit      *int64
	ctes       []cte
	settings   []settingKV
}

// NewQuery returns a new empty Query.
func NewQuery() *Query {
	return &Query{}
}

// Select appends SELECT expressions (no positional args).
func (q *Query) Select(exprs ...string) *Query {
	q.selects = append(q.selects, exprs...)
	return q
}

// SelectExpr appends a SELECT expression that contains positional ? args.
// Args are emitted after CTE args and before WHERE args, matching ClickHouse
// document order. Used for expressions like windowFunnel()(occur_time, kind = ?).
func (q *Query) SelectExpr(expr string, args ...any) *Query {
	q.selects = append(q.selects, expr)
	q.selectArgs = append(q.selectArgs, args...)
	return q
}

// From sets the FROM clause (table name, or a full FROM expression including JOINs).
func (q *Query) From(table string) *Query {
	q.from = table
	return q
}

// Where appends conditions. All conditions across multiple Where calls are AND-joined.
// Zero-value conditions are silently skipped.
func (q *Query) Where(conds ...Condition) *Query {
	for _, c := range conds {
		if !c.IsZero() {
			q.wheres = append(q.wheres, c)
		}
	}
	return q
}

// GroupBy appends GROUP BY columns.
func (q *Query) GroupBy(cols ...string) *Query {
	q.groupBy = append(q.groupBy, cols...)
	return q
}

// OrderBy appends ORDER BY expressions.
func (q *Query) OrderBy(exprs ...string) *Query {
	q.orderBy = append(q.orderBy, exprs...)
	return q
}

// Limit sets the LIMIT value.
func (q *Query) Limit(n int64) *Query {
	q.limit = &n
	return q
}

// WithQueryCache enables the ClickHouse query cache with the given TTL in seconds.
//
// Apply only to the outermost query. The Pug builder rejects SETTINGS on any
// non-outermost query (CTE sub-query, UNION branch, or any future nesting form);
// calling WithQueryCache on such a query causes the outer Build() to panic.
// Repeated calls overwrite the previous TTL — the SETTINGS clause always contains
// exactly one (use_query_cache, query_cache_ttl) pair.
//
// Panics if ttlSeconds is not positive.
func (q *Query) WithQueryCache(ttlSeconds int) *Query {
	if ttlSeconds <= 0 {
		panic(fmt.Sprintf("clickhouse: Query.WithQueryCache: ttlSeconds must be positive, got %d", ttlSeconds))
	}
	q.settings = upsertSetting(q.settings, intSetting("use_query_cache", 1))
	q.settings = upsertSetting(q.settings, intSetting("query_cache_ttl", ttlSeconds))
	return q
}

// With adds a named CTE. The sub-query's args are emitted before the main query's args.
// Panics if sub is nil — pass a non-nil *Query or guard with a nil check before calling.
//
// The sub-query is held by reference; do not mutate it after passing it here. In
// particular, calling sub.WithQueryCache(...) after With() will cause this query's
// Build() to panic, since SETTINGS are only valid on the outermost query.
func (q *Query) With(name string, sub *Query) *Query {
	if sub == nil {
		panic("clickhouse: With called with nil sub-query for CTE " + name)
	}
	q.ctes = append(q.ctes, cte{name: name, sub: sub})
	return q
}

// Build assembles the SQL string and positional args.
// CTE args are emitted before main query args, matching ClickHouse document order.
func (q *Query) Build() (string, []any, error) {
	var sb strings.Builder
	var args []any

	// CTEs
	for i, c := range q.ctes {
		if len(c.sub.settings) > 0 {
			panic(fmt.Sprintf("clickhouse: cte %q: SETTINGS is only allowed on the outermost query — call WithQueryCache on the outer query, not the CTE sub-query", c.name))
		}
		subSQL, subArgs, err := c.sub.Build()
		if err != nil {
			return "", nil, fmt.Errorf("cte %q: %w", c.name, err)
		}
		if i == 0 {
			sb.WriteString("WITH ")
		} else {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "%s AS (\n%s\n)\n", c.name, subSQL)
		args = append(args, subArgs...)
	}

	// SELECT
	if len(q.selects) == 0 {
		return "", nil, fmt.Errorf("SELECT expressions are required")
	}
	sb.WriteString("SELECT ")
	sb.WriteString(strings.Join(q.selects, ",\n"))
	sb.WriteString("\n")
	args = append(args, q.selectArgs...)

	// FROM
	if q.from == "" {
		return "", nil, fmt.Errorf("FROM table is required")
	}
	sb.WriteString("FROM ")
	sb.WriteString(q.from)
	sb.WriteString("\n")

	// WHERE
	active := filterZero(q.wheres)
	for i, c := range active {
		if i == 0 {
			sb.WriteString("WHERE ")
		} else {
			sb.WriteString("AND ")
		}
		sb.WriteString(c.sql)
		sb.WriteString("\n")
		args = append(args, c.args...)
	}

	// GROUP BY
	if len(q.groupBy) > 0 {
		sb.WriteString("GROUP BY ")
		sb.WriteString(strings.Join(q.groupBy, ", "))
		sb.WriteString("\n")
	}

	// ORDER BY
	if len(q.orderBy) > 0 {
		sb.WriteString("ORDER BY ")
		sb.WriteString(strings.Join(q.orderBy, ", "))
		sb.WriteString("\n")
	}

	// LIMIT
	if q.limit != nil {
		sb.WriteString("LIMIT ?")
		args = append(args, *q.limit)
	}

	sql := strings.TrimRight(sb.String(), "\n")

	if len(q.settings) > 0 {
		sql += "\nSETTINGS " + formatSettings(q.settings)
	}

	return sql, args, nil
}

func formatSettings(s []settingKV) string {
	parts := make([]string, len(s))
	for i, kv := range s {
		parts[i] = kv.format()
	}
	return strings.Join(parts, ", ")
}

// UnionQuery composes multiple Queries with UNION ALL.
type UnionQuery struct {
	queries  []*Query
	orderBy  []string
	settings []settingKV
}

// UnionAll creates a new UnionQuery from the given queries.
// Panics if any query is nil — pass non-nil *Query values or guard with a nil check before calling.
//
// Each query is held by reference; do not mutate one after passing it here. In particular,
// calling q.WithQueryCache(...) on an inner query after UnionAll() will cause Build() to panic,
// since SETTINGS are only valid on the outermost UnionQuery.
func UnionAll(queries ...*Query) *UnionQuery {
	for i, q := range queries {
		if q == nil {
			panic(fmt.Sprintf("clickhouse: UnionAll called with nil query at index %d", i))
		}
	}
	return &UnionQuery{queries: queries}
}

// OrderBy sets the ORDER BY for the outer UNION ALL query.
func (u *UnionQuery) OrderBy(exprs ...string) *UnionQuery {
	u.orderBy = append(u.orderBy, exprs...)
	return u
}

// WithQueryCache enables the ClickHouse query cache with the given TTL in seconds.
//
// Apply only to the outermost UnionQuery. The Pug builder rejects SETTINGS on any
// non-outermost query; calling WithQueryCache on a Query passed to UnionAll(...) causes
// the outer Build() to panic. Repeated calls overwrite the previous TTL — the SETTINGS
// clause always contains exactly one (use_query_cache, query_cache_ttl) pair.
//
// Panics if ttlSeconds is not positive.
func (u *UnionQuery) WithQueryCache(ttlSeconds int) *UnionQuery {
	if ttlSeconds <= 0 {
		panic(fmt.Sprintf("clickhouse: UnionQuery.WithQueryCache: ttlSeconds must be positive, got %d", ttlSeconds))
	}
	u.settings = upsertSetting(u.settings, intSetting("use_query_cache", 1))
	u.settings = upsertSetting(u.settings, intSetting("query_cache_ttl", ttlSeconds))
	return u
}

// Build assembles the UNION ALL SQL and positional args.
func (u *UnionQuery) Build() (string, []any, error) {
	if len(u.queries) == 0 {
		return "", nil, fmt.Errorf("UnionAll requires at least one query")
	}

	parts := make([]string, len(u.queries))
	var args []any
	for i, q := range u.queries {
		if len(q.settings) > 0 {
			panic(fmt.Sprintf("clickhouse: query %d: SETTINGS is only allowed on the outermost UnionQuery — call WithQueryCache on the UnionQuery, not the inner Query", i))
		}
		sql, qArgs, err := q.Build()
		if err != nil {
			return "", nil, fmt.Errorf("query %d: %w", i, err)
		}
		parts[i] = sql
		args = append(args, qArgs...)
	}

	var sb strings.Builder
	sb.WriteString(strings.Join(parts, "\nUNION ALL\n"))

	if len(u.orderBy) > 0 {
		sb.WriteString("\nORDER BY ")
		sb.WriteString(strings.Join(u.orderBy, ", "))
	}

	if len(u.settings) > 0 {
		sb.WriteString("\nSETTINGS ")
		sb.WriteString(formatSettings(u.settings))
	}

	return sb.String(), args, nil
}
