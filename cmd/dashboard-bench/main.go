// dashboard-bench measures end-to-end ClickHouse query latency for every tile
// in the trends, funnel, or retention stress dashboard. It builds each tile's query via the
// same code path the RPC handler uses, executes it against a live ClickHouse,
// and reports cold + warm timings.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"
	"golang.org/x/sync/errgroup"

	pgseed "github.com/pug-sh/pug/internal/app/seed/postgres"
	coreinsights "github.com/pug-sh/pug/internal/core/insights"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
)

func main() {
	chDSN := flag.String("ch", "clickhouse://default:@localhost:9000/pug", "ClickHouse DSN")
	projectID := flag.String("project", "", "project ID to bind queries against")
	kind := flag.String("kind", "trends", "tile set to benchmark: trends, funnel, or retention")
	runs := flag.Int("runs", 3, "warm runs per tile (after one cold run)")
	dropCache := flag.Bool("drop-cache", true, "drop the query cache before every run (raw timing)")
	dumpSQL := flag.Bool("dump-sql", false, "print compiled SQL for each tile")
	flag.Parse()

	if *projectID == "" {
		fmt.Fprintln(os.Stderr, "ERROR: -project is required")
		os.Exit(2)
	}

	ctx := context.Background()
	opts, err := ch.ParseDSN(*chDSN)
	if err != nil {
		die("parse DSN: %v", err)
	}
	conn, err := ch.Open(opts)
	if err != nil {
		die("open: %v", err)
	}
	if err := conn.Ping(ctx); err != nil {
		die("ping: %v", err)
	}
	defer conn.Close() //nolint:errcheck

	executor := coreinsights.NewExecutor(conn)

	now := time.Now().UTC()
	var tiles []pgseed.TileForBench
	switch *kind {
	case "trends":
		tiles = pgseed.StressTrendsTilesForBench(now)
	case "funnel":
		tiles = pgseed.StressFunnelTilesForBench(now)
	case "retention":
		tiles = pgseed.StressRetentionTilesForBench(now)
	default:
		die("unknown -kind %q (use trends, funnel, or retention)", *kind)
	}

	fmt.Printf("Benchmarking %d %s tiles against project %s\n\n", len(tiles), *kind, *projectID)

	header := "%-2s  %-44s  %8s  %8s  %8s  %8s  %s\n"
	row := "%-2d  %-44s  %8s  %8s  %8s  %8s  %s\n"
	fmt.Printf(header, "#", "Tile", "cold", "warm/avg", "warm/p95", "warm/best", "verdict")
	fmt.Printf(header, "--", "------------------------------------------", "--------", "--------", "--------", "--------", "-------")

	allPass := true
	for i, t := range tiles {
		if *dumpSQL {
			dumpTileSQL(t, *projectID)
		}

		if *dropCache {
			_ = conn.Exec(ctx, "SYSTEM DROP QUERY CACHE")
		}
		cold, err := timeTile(ctx, executor, *projectID, t)
		if err != nil {
			fmt.Printf(row, i+1, truncate(t.DisplayName(), 44), "ERR", "-", "-", "-", err.Error())
			allPass = false
			continue
		}

		samples := make([]time.Duration, 0, *runs)
		for r := 0; r < *runs; r++ {
			if *dropCache {
				_ = conn.Exec(ctx, "SYSTEM DROP QUERY CACHE")
			}
			d, err := timeTile(ctx, executor, *projectID, t)
			if err != nil {
				fmt.Printf(row, i+1, truncate(t.DisplayName(), 44), fmtDur(cold), "ERR", "-", "-", err.Error())
				allPass = false
				continue
			}
			samples = append(samples, d)
		}

		avg, p95, best := stats(samples)
		verdict := "PASS"
		if avg > 500*time.Millisecond {
			verdict = "FAIL (>500ms warm avg)"
			allPass = false
		}
		fmt.Printf(row, i+1, truncate(t.DisplayName(), 44),
			fmtDur(cold), fmtDur(avg), fmtDur(p95), fmtDur(best), verdict)
	}

	fmt.Println()
	if allPass {
		fmt.Println("All tiles passed <500ms warm avg target.")
		return
	}
	fmt.Println("Some tiles exceeded the 500ms warm avg target.")
	os.Exit(1)
}

func timeTile(ctx context.Context, exec *coreinsights.Executor, projectID string, t pgseed.TileForBench) (time.Duration, error) {
	req := t.Query()
	start := time.Now()
	switch req.GetInsightType() {
	case insightsv1.InsightType_INSIGHT_TYPE_TRENDS:
		q, err := coreinsights.BuildTrendsQuery(req, projectID)
		if err != nil {
			return 0, err
		}
		_, err = exec.QueryTrends(ctx, projectID, q)
		return time.Since(start), err

	case insightsv1.InsightType_INSIGHT_TYPE_FUNNEL:
		if req.GetIncludeStepTiming() {
			return timeFunnelWithTiming(ctx, exec, projectID, req)
		}
		q, err := coreinsights.BuildFunnelCountsQuery(req, projectID)
		if err != nil {
			return 0, err
		}
		_, err = exec.QueryFunnel(ctx, projectID, q)
		return time.Since(start), err

	case insightsv1.InsightType_INSIGHT_TYPE_RETENTION:
		q, err := coreinsights.BuildRetentionQuery(req, projectID)
		if err != nil {
			return 0, err
		}
		_, err = exec.QueryRetention(ctx, projectID, q)
		return time.Since(start), err

	default:
		return 0, fmt.Errorf("unsupported insight type %v", req.GetInsightType())
	}
}

func timeFunnelWithTiming(ctx context.Context, exec *coreinsights.Executor, projectID string, req *insightsv1.QueryRequest) (time.Duration, error) {
	start := time.Now()

	countsQ, err := coreinsights.BuildFunnelCountsQuery(req, projectID)
	if err != nil {
		return 0, err
	}
	timingQ, err := coreinsights.BuildFunnelTimingQuery(req, projectID)
	if err != nil {
		return 0, err
	}

	var countRows []coreinsights.FunnelRow
	var timingRows []coreinsights.FunnelRow
	eg, egCtx := errgroup.WithContext(ctx)

	eg.Go(func() error {
		rows, err := exec.QueryFunnel(egCtx, projectID, countsQ)
		if err != nil {
			return err
		}
		countRows = rows
		return nil
	})

	eg.Go(func() error {
		users, err := exec.QueryFunnelUserEvents(egCtx, projectID, timingQ)
		if err != nil {
			return err
		}
		rows, err := coreinsights.ComputeFunnelTiming(egCtx, projectID, users, timingQ.Kinds(), timingQ.WindowSec(), timingQ.NumBreakdowns())
		if err != nil {
			return err
		}
		timingRows = rows
		return nil
	})

	if err := eg.Wait(); err != nil {
		return 0, err
	}
	_ = coreinsights.MergeFunnelCountsAndTiming(countRows, timingRows)
	return time.Since(start), nil
}

func dumpTileSQL(t pgseed.TileForBench, projectID string) {
	req := t.Query()
	switch req.GetInsightType() {
	case insightsv1.InsightType_INSIGHT_TYPE_TRENDS:
		q, err := coreinsights.BuildTrendsQuery(req, projectID)
		if err != nil {
			fmt.Printf("\n--- Tile: %s (build error: %v) ---\n", t.DisplayName(), err)
			return
		}
		fmt.Printf("\n--- Tile: %s (trends) ---\n%s\n%v\n", t.DisplayName(), q.SQL(), q.Args())
	case insightsv1.InsightType_INSIGHT_TYPE_FUNNEL:
		countsQ, err := coreinsights.BuildFunnelCountsQuery(req, projectID)
		if err != nil {
			fmt.Printf("\n--- Tile: %s (counts build error: %v) ---\n", t.DisplayName(), err)
			return
		}
		fmt.Printf("\n--- Tile: %s (funnel counts) ---\n%s\n%v\n", t.DisplayName(), countsQ.SQL(), countsQ.Args())
		if req.GetIncludeStepTiming() {
			timingQ, err := coreinsights.BuildFunnelTimingQuery(req, projectID)
			if err != nil {
				fmt.Printf("--- Tile: %s (timing build error: %v) ---\n", t.DisplayName(), err)
				return
			}
			fmt.Printf("--- Tile: %s (funnel timing) ---\n%s\n%v\n", t.DisplayName(), timingQ.SQL(), timingQ.Args())
		}
	case insightsv1.InsightType_INSIGHT_TYPE_RETENTION:
		q, err := coreinsights.BuildRetentionQuery(req, projectID)
		if err != nil {
			fmt.Printf("\n--- Tile: %s (build error: %v) ---\n", t.DisplayName(), err)
			return
		}
		fmt.Printf("\n--- Tile: %s (retention) ---\n%s\n%v\n", t.DisplayName(), q.SQL(), q.Args())
	}
}

func stats(s []time.Duration) (avg, p95, best time.Duration) {
	if len(s) == 0 {
		return 0, 0, 0
	}
	var sum time.Duration
	best = s[0]
	for _, d := range s {
		sum += d
		if d < best {
			best = d
		}
	}
	avg = sum / time.Duration(len(s))

	sorted := make([]time.Duration, len(s))
	copy(sorted, s)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j-1] > sorted[j]; j-- {
			sorted[j-1], sorted[j] = sorted[j], sorted[j-1]
		}
	}
	idx := int(float64(len(sorted))*0.95 + 0.5)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	p95 = sorted[idx]
	return avg, p95, best
}

func fmtDur(d time.Duration) string {
	if d < time.Millisecond {
		return fmt.Sprintf("%.2fms", float64(d.Microseconds())/1000.0)
	}
	return fmt.Sprintf("%dms", d.Milliseconds())
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func die(format string, args ...any) {
	fmt.Fprintln(os.Stderr, "ERROR: "+fmt.Sprintf(format, args...))
	os.Exit(1)
}
