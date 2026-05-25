// dashboard-api-bench hits the live Insights Query RPC for each stress tile.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	pgseed "github.com/pug-sh/pug/internal/app/seed/postgres"
)

func main() {
	baseURL := flag.String("url", "http://localhost:3000", "server base URL")
	projectID := flag.String("project", "", "project ID")
	apiKey := flag.String("key", "", "private API key")
	kind := flag.String("kind", "funnel", "trends, funnel, or retention")
	flag.Parse()

	if *projectID == "" || *apiKey == "" {
		fmt.Fprintln(os.Stderr, "ERROR: -project and -key are required")
		os.Exit(2)
	}

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
		die("unknown -kind %q", *kind)
	}

	m := protojson.MarshalOptions{UseProtoNames: false}
	client := &http.Client{Timeout: 2 * time.Minute}
	endpoint := *baseURL + "/shared.insights.v1.InsightsService/Query"

	fmt.Printf("API benchmark: %d %s tiles\n\n", len(tiles), *kind)
	allPass := true
	for i, t := range tiles {
		body, err := m.Marshal(t.Query())
		if err != nil {
			die("marshal tile %d: %v", i+1, err)
		}

		start := time.Now()
		req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			die("request tile %d: %v", i+1, err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Connect-Protocol-Version", "1")
		req.Header.Set("x-api-key", *apiKey)
		req.Header.Set("x-project-id", *projectID)

		resp, err := client.Do(req)
		elapsed := time.Since(start)
		if err != nil {
			fmt.Printf("%d  %-44s  ERR  %v\n", i+1, truncate(t.DisplayName(), 44), err)
			allPass = false
			continue
		}
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		resp.Body.Close()

		verdict := "PASS"
		if resp.StatusCode != http.StatusOK {
			verdict = fmt.Sprintf("HTTP %d", resp.StatusCode)
			allPass = false
		} else if elapsed > 500*time.Millisecond {
			verdict = "SLOW (>500ms)"
			allPass = false
		}
		fmt.Printf("%d  %-44s  %4dms  %s\n", i+1, truncate(t.DisplayName(), 44), elapsed.Milliseconds(), verdict)
	}

	if !allPass {
		os.Exit(1)
	}
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
