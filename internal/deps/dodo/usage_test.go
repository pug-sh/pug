package dodo

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
)

func TestIngestUsageSendsOneEventWithAKeyPerTier(t *testing.T) {
	var path string
	var body struct {
		Events []struct {
			CustomerID string             `json:"customer_id"`
			EventID    string             `json:"event_id"`
			EventName  string             `json:"event_name"`
			Timestamp  time.Time          `json:"timestamp"`
			Metadata   map[string]float64 `json:"metadata"`
		} `json:"events"`
	}
	c := apiClient(t, jsonHandler(t, http.StatusOK, `{"ingested_count":1}`, func(r *http.Request) {
		path = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode ingest request: %v", err)
		}
	}))
	at := time.Date(2026, 10, 20, 13, 0, 0, 0, time.UTC)
	err := c.IngestUsage(context.Background(), corebilling.UsageStatement{
		CustomerID: "cus_1", EventID: "pug_abc", At: at, TierEvents: []int64{1_900_000, 13_000_000, 0},
	})
	if err != nil {
		t.Fatalf("IngestUsage: %v", err)
	}
	if path != "/events/ingest" {
		t.Errorf("path = %s, want /events/ingest", path)
	}
	if len(body.Events) != 1 {
		t.Fatalf("sent %d events, want 1", len(body.Events))
	}
	ev := body.Events[0]
	if ev.CustomerID != "cus_1" || ev.EventID != "pug_abc" || ev.EventName != "pug.usage" || !ev.Timestamp.Equal(at) {
		t.Errorf("event = %+v", ev)
	}
	want := map[string]float64{"t1": 1_900_000, "t2": 13_000_000, "t3": 0}
	for k, v := range want {
		if got, ok := ev.Metadata[k]; !ok || got != v {
			t.Errorf("metadata[%s] = %v (present %v), want %v", k, got, ok, v)
		}
	}
}

func TestIngestUsageWrapsAFailure(t *testing.T) {
	c := apiClient(t, jsonHandler(t, http.StatusUnprocessableEntity, `{"message":"bad"}`, nil))
	err := c.IngestUsage(context.Background(), corebilling.UsageStatement{CustomerID: "c", EventID: "e", At: time.Now(), TierEvents: []int64{1}})
	if err == nil || !strings.HasPrefix(err.Error(), "dodo: ingest usage:") {
		t.Fatalf("err = %v, want a wrapped ingest failure", err)
	}
}

const meterList = `{"items":[
 {"id":"mtr_1","event_name":"pug.usage","aggregation":{"type":"max","key":"t1"}},
 {"id":"mtr_2","event_name":"pug.usage","aggregation":{"type":"max","key":"t2"}},
 {"id":"mtr_x","event_name":"other","aggregation":{"type":"sum","key":"t1"}}]}`

func usageProductJSON(fixed int64, meters string) string {
	return `{"product_id":"prod_1","price":{"type":"usage_based_price","currency":"USD","fixed_price":` +
		strconv.FormatInt(fixed, 10) + `,"meters":` + meters + `}}`
}

func TestVerifyMetering(t *testing.T) {
	good := `[{"meter_id":"mtr_1","free_threshold":0,"price_per_unit":"0.0011"},{"meter_id":"mtr_2","free_threshold":0,"price_per_unit":"0.0009"}]`
	cases := []struct {
		name, product, wantErr string
		tiers                  int
	}{
		{"a product billing both tiers", usageProductJSON(100, good), "", 2},
		{"a third tier with no meter", usageProductJSON(100, good), "no max meter", 3},
		{"a product missing a tier's meter", usageProductJSON(100, `[{"meter_id":"mtr_1","free_threshold":0}]`), "does not attach tier 2", 2},
		{"a free threshold of its own", usageProductJSON(100, `[{"meter_id":"mtr_1","free_threshold":0},{"meter_id":"mtr_2","free_threshold":500}]`), "free threshold", 2},
		{"a fee under a dollar", usageProductJSON(50, good), "fixed fee", 2},
		{"a product priced some other way", `{"product_id":"prod_1","price":{"type":"recurring_price","currency":"USD","price":1000}}`, "not usage-priced", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.Handle("/meters", meterPages(t, nil))
			mux.Handle("/products/prod_1", jsonHandler(t, http.StatusOK, tc.product, nil))
			err := apiClient(t, mux).VerifyMetering(context.Background(), tc.tiers, []string{"prod_1"})
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("VerifyMetering: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

// Dodo counts pages from 0. Left to itself the SDK's pager takes an unnumbered
// first request for page 1 and asks for page 2 next, so a second page of meters
// would never be read.
func TestVerifyMeteringReadsEveryPageOfMeters(t *testing.T) {
	var pages []string
	mux := http.NewServeMux()
	mux.Handle("/meters", meterPages(t, &pages))
	mux.Handle("/products/prod_1", jsonHandler(t, http.StatusOK, usageProductJSON(100,
		`[{"meter_id":"mtr_1","free_threshold":0},{"meter_id":"mtr_2","free_threshold":0}]`), nil))
	if err := apiClient(t, mux).VerifyMetering(context.Background(), 2, []string{"prod_1"}); err != nil {
		t.Fatalf("VerifyMetering: %v", err)
	}
	if want := []string{"0", "1"}; !slices.Equal(pages, want) {
		t.Errorf("pages requested = %v, want %v", pages, want)
	}
}

// meterPages serves meterList as page 0 and an empty page after it, which is where
// the pager stops.
func meterPages(t *testing.T, pages *[]string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page_number")
		if pages != nil {
			*pages = append(*pages, page)
		}
		body := `{"items":[]}`
		if page == "0" {
			body = meterList
		}
		jsonHandler(t, http.StatusOK, body, nil).ServeHTTP(w, r)
	})
}
