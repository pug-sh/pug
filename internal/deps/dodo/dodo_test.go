package dodo

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
)

const testSecret = "whsec_" // + the base64 key below

var secretKey = []byte("0123456789abcdef0123456789abcdef")

func secret() string { return testSecret + base64.StdEncoding.EncodeToString(secretKey) }

// signed builds a delivery the way Standard Webhooks specifies: HMAC-SHA256 over
// "{id}.{timestamp}.{body}". Written out rather than taken from the library, which
// would only agree with itself.
func signed(id string, at time.Time, body []byte) http.Header {
	ts := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, secretKey)
	fmt.Fprintf(mac, "%s.%s.%s", id, ts, body)
	h := http.Header{}
	h.Set(headerWebhookID, id)
	h.Set(headerWebhookTimestamp, ts)
	h.Set(headerWebhookSignature, "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	return h
}

func testClient(t *testing.T, now time.Time) *Client {
	t.Helper()
	c, err := New(Config{APIKey: "sk_test", Environment: EnvironmentTest, WebhookSecret: secret()},
		map[string]string{"growth": "prod_growth"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.verifier.now = func() time.Time { return now }
	return c
}

const activeBody = `{"type":"subscription.active","data":{` +
	`"subscription_id":"sub_1","product_id":"prod_growth","status":"active",` +
	`"currency":"USD","recurring_pre_tax_amount":2000,` +
	`"customer":{"customer_id":"cus_1"},` +
	`"metadata":{"org_id":"org_abc","checkout_ref":"ref_deadbeef"},` +
	`"previous_billing_date":"2026-06-01T00:00:00Z","next_billing_date":"2026-07-01T00:00:00Z"}}`

func TestVerify(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	body := []byte(activeBody)

	t.Run("good vector", func(t *testing.T) {
		c := testClient(t, now)
		d, err := c.Verify(signed("evt_1", now, body), body)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if d.WebhookID != "evt_1" {
			t.Errorf("webhook_id = %q, want evt_1", d.WebhookID)
		}
		if d.EventType != "subscription.active" {
			t.Errorf("event_type = %q — it must be read from the SIGNED body, not a header", d.EventType)
		}
		if !d.DeliveredAt.Equal(now.Truncate(time.Second)) {
			t.Errorf("delivered_at = %s, want %s", d.DeliveredAt, now)
		}
	})

	t.Run("tampered body", func(t *testing.T) {
		c := testClient(t, now)
		headers := signed("evt_1", now, body)
		tampered := []byte(`{"type":"subscription.active","data":{"subscription_id":"sub_evil"}}`)
		if _, err := c.Verify(headers, tampered); err == nil {
			t.Fatal("a tampered body verified")
		}
	})

	t.Run("stale timestamp", func(t *testing.T) {
		c := testClient(t, now)
		old := now.Add(-tolerance - time.Second)
		if _, err := c.Verify(signed("evt_1", old, body), body); err == nil {
			t.Fatal("a delivery outside the tolerance window verified")
		}
	})

	t.Run("future timestamp", func(t *testing.T) {
		c := testClient(t, now)
		ahead := now.Add(tolerance + time.Second)
		if _, err := c.Verify(signed("evt_1", ahead, body), body); err == nil {
			t.Fatal("a delivery from the future verified")
		}
	})

	t.Run("multiple signatures", func(t *testing.T) {
		// A secret rotation puts several space-delimited signatures in the header;
		// verifying must accept the one that matches, wherever it sits.
		c := testClient(t, now)
		headers := signed("evt_1", now, body)
		headers.Set(headerWebhookSignature, "v1,bm90LWEtc2lnbmF0dXJl "+headers.Get(headerWebhookSignature))
		if _, err := c.Verify(headers, body); err != nil {
			t.Fatalf("a rotating header with a valid signature was rejected: %v", err)
		}
	})

	t.Run("missing headers", func(t *testing.T) {
		c := testClient(t, now)
		headers := signed("evt_1", now, body)
		headers.Del(headerWebhookSignature)
		if _, err := c.Verify(headers, body); err == nil {
			t.Fatal("a delivery with no signature header verified")
		}
	})

	t.Run("no secret configured verifies nothing", func(t *testing.T) {
		c, err := New(Config{APIKey: "sk_test"}, nil)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if c.CanVerify() {
			t.Fatal("CanVerify with no secret; the route must not mount")
		}
		if _, err := c.Verify(signed("evt_1", now, body), body); err == nil {
			t.Fatal("verified with no secret configured")
		}
	})
}

func TestNormalize(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	c := testClient(t, now)
	body := []byte(activeBody)

	d, err := c.Verify(signed("evt_1", now, body), body)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	event, err := c.Normalize(d)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if event.ProviderSubID != "sub_1" || event.ProviderCustomerID != "cus_1" {
		t.Errorf("ids = (%q, %q), want (sub_1, cus_1)", event.ProviderSubID, event.ProviderCustomerID)
	}
	// The ref is what attributes; org_id counts only beside a staged product.
	if event.CheckoutRef != "ref_deadbeef" {
		t.Errorf("checkout_ref = %q, want ref_deadbeef", event.CheckoutRef)
	}
	if event.OrgID != "org_abc" {
		t.Errorf("org_id = %q, want org_abc — attribution comes from metadata", event.OrgID)
	}
	if event.Status != corebilling.SubStatusActive {
		t.Errorf("status = %q, want active", event.Status)
	}
	if event.PriceCents != 2000 || event.Currency != "USD" {
		t.Errorf("price = (%d, %q), want (2000, USD)", event.PriceCents, event.Currency)
	}
	if event.ProductID != "prod_growth" {
		t.Errorf("product_id = %q, want prod_growth", event.ProductID)
	}
	wantEnd := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if !event.CurrentPeriodEnd.Equal(wantEnd) {
		t.Errorf("current_period_end = %s, want %s", event.CurrentPeriodEnd, wantEnd)
	}
}

// Dodo's metadata is string|number|bool. Decoding into map[string]string would fail
// the whole delivery over one numeric value, and a rejection is never retried.
func TestNormalizeKeepsAttributionBesideNonStringMetadata(t *testing.T) {
	c := testClient(t, time.Now())
	body := `{"type":"subscription.active","data":{` +
		`"subscription_id":"sub_1","product_id":"prod_growth","status":"active",` +
		`"currency":"USD","recurring_pre_tax_amount":2000,` +
		`"customer":{"customer_id":"cus_1"},` +
		`"metadata":{"org_id":"org_abc","checkout_ref":"ref_deadbeef","seats":5,"trial":true}}}`

	event, err := c.Normalize(corebilling.Delivery{
		EventType:  "subscription.active",
		RawPayload: []byte(body),
	})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if event.OrgID != "org_abc" {
		t.Errorf("org_id = %q, want org_abc", event.OrgID)
	}
	if event.CheckoutRef != "ref_deadbeef" {
		t.Errorf("checkout_ref = %q, want ref_deadbeef", event.CheckoutRef)
	}
}

// A payment, a refund and an event type that does not exist yet all normalize to
// nothing. They must never 500 and never retry forever.
func TestNormalizeIgnoresNonSubscriptionDeliveries(t *testing.T) {
	c := testClient(t, time.Now())
	for _, eventType := range []string{"payment.succeeded", "refund.succeeded", "dispute.opened", "some.future.thing"} {
		event, err := c.Normalize(corebilling.Delivery{
			EventType:  eventType,
			RawPayload: []byte(`{"type":"` + eventType + `","data":{"payment_id":"pay_1"}}`),
		})
		if err != nil {
			t.Errorf("%s: Normalize returned an error: %v", eventType, err)
		}
		if !event.IsZero() {
			t.Errorf("%s normalized to something applicable: %+v", eventType, event)
		}
	}
}

// Section 7's mapping table, in full. The last row is the one that matters: a
// state pug has no word for is kept verbatim and is not live.
func TestStatusMapping(t *testing.T) {
	cases := map[string]struct {
		want corebilling.SubStatus
		live bool
	}{
		"active":    {corebilling.SubStatusActive, true},
		"on_hold":   {corebilling.SubStatusPastDue, true},
		"paused":    {corebilling.SubStatusPaused, false},
		"cancelled": {corebilling.SubStatusCancelled, false},
		"expired":   {corebilling.SubStatusExpired, false},
		"failed":    {corebilling.SubStatusFailed, false},
		// Dodo has this state and pug has no word for it.
		"pending":            {"pending", false},
		"something_invented": {"something_invented", false},
	}
	for in, want := range cases {
		got := statusFromDodo(in)
		if got != want.want {
			t.Errorf("statusFromDodo(%q) = %q, want %q", in, got, want.want)
		}
		if got.Live() != want.live {
			t.Errorf("statusFromDodo(%q).Live() = %v, want %v", in, got.Live(), want.live)
		}
		if _, known := corebilling.ParseSubStatus(string(got)); known != want.live && !want.live {
			// Only the unmapped ones must fail to parse; paused/cancelled/expired/failed
			// are pug's own words and parse fine while still not being live.
			if in == "pending" || in == "something_invented" {
				t.Errorf("ParseSubStatus(%q) accepted an unmapped provider state", got)
			}
		}
	}
}

func TestProductIDs(t *testing.T) {
	env := map[string]string{
		"PUG_DODO_PRODUCT_STARTER": "prod_s",
		"PUG_DODO_PRODUCT_GROWTH":  "prod_g",
		// No SCALE key: a tier with no product is simply not purchasable.
		"PUG_DODO_PRODUCT_CUSTOM": "prod_never_read",
		"PUG_DODO_PRODUCT_FREE":   "prod_never_read",
	}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }

	got, err := ProductIDs(lookup)
	if err != nil {
		t.Fatalf("ProductIDs: %v", err)
	}
	want := map[string]string{"starter": "prod_s", "growth": "prod_g"}
	if len(got) != len(want) {
		t.Fatalf("ProductIDs = %v, want %v", got, want)
	}
	for slug, id := range want {
		if got[slug] != id {
			t.Errorf("ProductIDs[%q] = %q, want %q", slug, got[slug], id)
		}
	}

	// Two tiers on one product makes an incoming subscription's tier ambiguous,
	// and the webhook would pick one silently.
	env["PUG_DODO_PRODUCT_SCALE"] = "prod_g"
	if _, err := ProductIDs(lookup); err == nil {
		t.Fatal("two tiers sharing a product id was accepted")
	}
}

// Only the floors and custom are excluded. A retired tier keeps its mapping, or the
// webhook could not place its existing holders' renewals and cancellations.
func TestMappedSlug(t *testing.T) {
	for _, tc := range []struct {
		plan corebilling.Plan
		want bool
	}{
		{corebilling.Plan{Slug: "growth"}, true},
		{corebilling.Plan{Slug: "growth-v0", Retired: true}, true},
		{corebilling.Plan{Slug: corebilling.SlugFree}, false},
		{corebilling.Plan{Slug: corebilling.SlugTrial}, false},
		{corebilling.Plan{Slug: corebilling.SlugCustom}, false},
	} {
		if got := mappedSlug(tc.plan); got != tc.want {
			t.Errorf("mappedSlug(%q, retired=%v) = %v, want %v",
				tc.plan.Slug, tc.plan.Retired, got, tc.want)
		}
	}
}

func TestNewRejectsAnUnknownEnvironment(t *testing.T) {
	if _, err := New(Config{APIKey: "sk", Environment: "staging"}, nil); err == nil {
		t.Fatal("an unknown environment was accepted; it must fail startup")
	}
	// No key at all is the self-hosted shape, not an error.
	c, err := New(Config{}, nil)
	if err != nil || c != nil {
		t.Fatalf("New with no key = (%v, %v), want (nil, nil)", c, err)
	}
}
