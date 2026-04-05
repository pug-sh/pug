package clickhouse

import (
	"fmt"
	"strings"
)

// Condition is an opaque SQL fragment with its positional args.
type Condition struct {
	sql  string
	args []any
	err  error
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
	return c.isZero()
}

// isZero reports whether the condition is a zero-value (should be skipped).
func (c Condition) isZero() bool {
	return c.sql == "" && c.err == nil
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
		if !c.isZero() {
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

// From sets the FROM table.
func (q *Query) From(table string) *Query {
	q.from = table
	return q
}

// Where appends conditions. All conditions across multiple Where calls are AND-joined.
// Zero-value conditions are silently skipped.
func (q *Query) Where(conds ...Condition) *Query {
	for _, c := range conds {
		if !c.isZero() {
			q.wheres = append(q.wheres, c)
		}
	}
	return q
}

// GroupBy sets the GROUP BY columns.
func (q *Query) GroupBy(cols ...string) *Query {
	q.groupBy = append(q.groupBy, cols...)
	return q
}

// OrderBy sets the ORDER BY expressions.
func (q *Query) OrderBy(exprs ...string) *Query {
	q.orderBy = append(q.orderBy, exprs...)
	return q
}

// Limit sets the LIMIT value.
func (q *Query) Limit(n int64) *Query {
	q.limit = &n
	return q
}

// With adds a named CTE. The sub-query's args are emitted before the main query's args.
func (q *Query) With(name string, sub *Query) *Query {
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
		if c.err != nil {
			return "", nil, c.err
		}
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

	return strings.TrimRight(sb.String(), "\n"), args, nil
}

// UnionQuery composes multiple Queries with UNION ALL.
type UnionQuery struct {
	queries []*Query
	orderBy []string
}

// UnionAll creates a new UnionQuery from the given queries.
func UnionAll(queries ...*Query) *UnionQuery {
	return &UnionQuery{queries: queries}
}

// OrderBy sets the ORDER BY for the outer UNION ALL query.
func (u *UnionQuery) OrderBy(exprs ...string) *UnionQuery {
	u.orderBy = append(u.orderBy, exprs...)
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

	return sb.String(), args, nil
}
