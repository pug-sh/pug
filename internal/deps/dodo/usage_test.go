package dodo

import (
	"context"
	"encoding/json"
	"errors"
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

// A 2xx is not proof the event is held: Dodo answers ingested_count 0 for an event
// it refused — one stamped by a clock running fast, say — and for a repeat it
// ignored, which it holds already. Only reading the event back tells them apart.
func TestIngestUsageConfirmsAnEventItDidNotIngest(t *testing.T) {
	statement := corebilling.UsageStatement{CustomerID: "cus_1", EventID: "pug_abc", At: time.Now(), TierEvents: []int64{1}}
	cases := []struct {
		name            string
		ingested        string
		get             http.Handler
		wantErr         string
		wantConfirmRead bool
	}{
		{"ingested", `{"ingested_count":1}`, nil, "", false},
		{"ignored as a repeat it holds", `{"ingested_count":0}`,
			jsonHandler(t, http.StatusOK, `{"event_id":"pug_abc","customer_id":"cus_1","event_name":"pug.usage","business_id":"b","timestamp":"2026-10-20T13:00:00Z"}`, nil),
			"", true},
		{"refused", `{"ingested_count":0}`, jsonHandler(t, http.StatusNotFound, `{"message":"not found"}`, nil),
			"does not hold it", true},
		{"unconfirmable", `{"ingested_count":0}`, jsonHandler(t, http.StatusInternalServerError, `{"message":"down"}`, nil),
			"confirm event pug_abc", true},
		{"more than was sent", `{"ingested_count":2}`, nil, "2 ingested", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			read := false
			mux := http.NewServeMux()
			mux.Handle("/events/ingest", jsonHandler(t, http.StatusOK, tc.ingested, nil))
			mux.Handle("/events/pug_abc", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				read = true
				if tc.get == nil {
					t.Error("read the event back after it was ingested")
					return
				}
				tc.get.ServeHTTP(w, r)
			}))
			err := apiClient(t, mux).IngestUsage(context.Background(), statement)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("IngestUsage: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
			if read != tc.wantConfirmRead {
				t.Errorf("read the event back = %v, want %v", read, tc.wantConfirmRead)
			}
		})
	}
}

// The tier meters, beside a sum over t1 of pug's own event and a max over t1 of
// another event: neither is tier 1's meter, and a product attaching either must be
// refused.
const meterList = `{"items":[
 {"id":"mtr_1","event_name":"pug.usage","aggregation":{"type":"max","key":"t1"}},
 {"id":"mtr_2","event_name":"pug.usage","aggregation":{"type":"max","key":"t2"}},
 {"id":"mtr_sum","event_name":"pug.usage","aggregation":{"type":"sum","key":"t1"}},
 {"id":"mtr_x","event_name":"other","aggregation":{"type":"max","key":"t1"}}]}`

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
		// A sum over t1 bills the total of every hourly statement, each of which is the
		// whole period so far.
		{"a sum meter beside the tiers", usageProductJSON(100,
			`[{"meter_id":"mtr_1","free_threshold":0},{"meter_id":"mtr_2","free_threshold":0},{"meter_id":"mtr_sum","free_threshold":0}]`),
			"attaches meter mtr_sum", 2},
		{"another event's meter beside the tiers", usageProductJSON(100,
			`[{"meter_id":"mtr_1","free_threshold":0},{"meter_id":"mtr_2","free_threshold":0},{"meter_id":"mtr_x","free_threshold":0}]`),
			"attaches meter mtr_x", 2},
		// Built for a plan of two tiers and checked against a plan of one: tier 2's
		// meter is a meter pug never states to.
		{"a meter past the plan's last tier", usageProductJSON(100, good), "attaches meter mtr_2", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.Handle("/meters", meterPages(t, meterList, nil))
			mux.Handle("/products/prod_1", jsonHandler(t, http.StatusOK, tc.product, nil))
			err := apiClient(t, mux).VerifyMetering(context.Background(), tc.tiers, []string{"prod_1"})
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("VerifyMetering: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			case tc.wantErr != "" && !errors.Is(err, corebilling.ErrMeteringMisconfigured):
				t.Fatalf("err = %v, want a finding wrapping ErrMeteringMisconfigured", err)
			}
		})
	}
}

// Two max meters on one tier's key would each bill the tier: refused before any
// product is read.
func TestVerifyMeteringRefusesTwoMetersOnOneKey(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("/meters", meterPages(t, `{"items":[
 {"id":"mtr_1","event_name":"pug.usage","aggregation":{"type":"max","key":"t1"}},
 {"id":"mtr_1b","event_name":"pug.usage","aggregation":{"type":"max","key":"t1"}}]}`, nil))
	err := apiClient(t, mux).VerifyMetering(context.Background(), 1, []string{"prod_1"})
	if !errors.Is(err, corebilling.ErrMeteringMisconfigured) || !strings.Contains(err.Error(), "both take the max") {
		t.Fatalf("err = %v, want two meters on one key refused", err)
	}
}

// Reconcile counts a finding and a failed read differently: an outage must not
// report every product it could not reach as misconfigured.
func TestVerifyMeteringTellsAFailedReadFromAFinding(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("/meters", meterPages(t, meterList, nil))
	mux.Handle("/products/prod_down", jsonHandler(t, http.StatusInternalServerError, `{"message":"down"}`, nil))
	mux.Handle("/products/prod_gone", jsonHandler(t, http.StatusNotFound, `{"message":"not found"}`, nil))
	c := apiClient(t, mux)
	if err := c.VerifyMetering(context.Background(), 2, []string{"prod_down"}); err == nil ||
		errors.Is(err, corebilling.ErrMeteringMisconfigured) {
		t.Errorf("a product read that failed: err = %v, want a plain error", err)
	}
	if err := c.VerifyMetering(context.Background(), 2, []string{"prod_gone"}); !errors.Is(err, corebilling.ErrMeteringMisconfigured) {
		t.Errorf("a product that does not exist: err = %v, want a finding", err)
	}
}

// Dodo counts pages from 0. Left to itself the SDK's pager takes an unnumbered
// first request for page 1 and asks for page 2 next, so a second page of meters
// would never be read.
func TestVerifyMeteringReadsEveryPageOfMeters(t *testing.T) {
	var pages []string
	mux := http.NewServeMux()
	mux.Handle("/meters", meterPages(t, meterList, &pages))
	mux.Handle("/products/prod_1", jsonHandler(t, http.StatusOK, usageProductJSON(100,
		`[{"meter_id":"mtr_1","free_threshold":0},{"meter_id":"mtr_2","free_threshold":0}]`), nil))
	if err := apiClient(t, mux).VerifyMetering(context.Background(), 2, []string{"prod_1"}); err != nil {
		t.Fatalf("VerifyMetering: %v", err)
	}
	if want := []string{"0", "1"}; !slices.Equal(pages, want) {
		t.Errorf("pages requested = %v, want %v", pages, want)
	}
}

// meterPages serves list as page 0 and an empty page after it, which is where the
// pager stops.
func meterPages(t *testing.T, list string, pages *[]string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page_number")
		if pages != nil {
			*pages = append(*pages, page)
		}
		body := `{"items":[]}`
		if page == "0" {
			body = list
		}
		jsonHandler(t, http.StatusOK, body, nil).ServeHTTP(w, r)
	})
}
