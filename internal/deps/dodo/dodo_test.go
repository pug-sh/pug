package dodo

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
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
	c, err := New(Config{APIKey: "sk_test", Environment: EnvironmentTest, WebhookSecret: secret()})
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

	// All three are checked in one condition, so each needs its own case or two of
	// the arms are carried by the third.
	for _, header := range []string{headerWebhookID, headerWebhookTimestamp, headerWebhookSignature} {
		t.Run("missing "+header, func(t *testing.T) {
			c := testClient(t, now)
			headers := signed("evt_1", now, body)
			headers.Del(header)
			if _, err := c.Verify(headers, body); err == nil {
				t.Fatalf("a delivery with no %s header verified", header)
			}
		})
	}

	t.Run("no secret configured verifies nothing", func(t *testing.T) {
		c, err := New(Config{APIKey: "sk_test"})
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

// subscriptionDelivery is a delivery of eventType for a subscription in status, its
// grace deadline written as deadline: a JSON value, or "" to leave the key out.
func subscriptionDelivery(eventType, status, deadline string) []byte {
	field := ""
	if deadline != "" {
		field = `,"past_due_ends_at":` + deadline
	}
	return []byte(`{"type":"` + eventType + `","data":{` +
		`"subscription_id":"sub_1","product_id":"prod_growth","status":"` + status + `",` +
		`"currency":"USD","recurring_pre_tax_amount":2000,` +
		`"customer":{"customer_id":"cus_1"},` +
		`"metadata":{"org_id":"org_abc","checkout_ref":"ref_deadbeef"},` +
		`"previous_billing_date":"2026-06-01T00:00:00Z","next_billing_date":"2026-07-01T00:00:00Z"` +
		field + `}}`)
}

// Dodo dates a grace window only in its own past_due, and on every subscription
// event sent inside one. Inside a window a deadline is believed only when it is
// there, parses and is plausible: anything else is doubt, which keeps the stored
// one and is reported, but never fails the delivery — the field only dates a
// banner. Outside a window there is no deadline, whatever the payload says.
func TestNormalizeTheGraceDeadline(t *testing.T) {
	sent := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	deadline := time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)
	const dated = `"2026-07-04T00:00:00Z"`
	cases := []struct {
		name, eventType, status, field string
		want                           time.Time
		known, issue                   bool
	}{
		{"inside a window", "subscription.past_due", "past_due", dated, deadline, true, false},
		{"a payment method update inside a window", "subscription.update_payment_method", "past_due", dated, deadline, true, false},
		{"a renewal inside a window", "subscription.renewed", "past_due", dated, deadline, true, false},
		{"an update inside a window", "subscription.updated", "past_due", dated, deadline, true, false},
		{"a plan change inside a window", "subscription.plan_changed", "past_due", dated, deadline, true, false},
		{"inside a window with the key missing", "subscription.past_due", "past_due", "", time.Time{}, false, true},
		{"inside a window with a null", "subscription.past_due", "past_due", `null`, time.Time{}, false, true},
		{"inside a window with an empty string", "subscription.past_due", "past_due", `""`, time.Time{}, false, true},
		{"inside a window with a date and no time", "subscription.past_due", "past_due", `"2026-07-04"`, time.Time{}, false, true},
		{"inside a window with a number", "subscription.past_due", "past_due", `1783123200`, time.Time{}, false, true},
		{"inside a window dated 1970", "subscription.past_due", "past_due", `"1970-01-01T00:00:00Z"`, time.Time{}, false, true},
		{"inside a window dated year 0", "subscription.past_due", "past_due", `"0000-01-01T00:00:00Z"`, time.Time{}, false, true},
		{"inside a window dated before it was sent", "subscription.past_due", "past_due", `"2026-06-29T00:00:00Z"`, time.Time{}, false, true},
		{"a hold", "subscription.on_hold", "on_hold", `null`, time.Time{}, true, false},
		{"a hold still carrying a deadline", "subscription.on_hold", "on_hold", dated, time.Time{}, true, true},
		{"active with the key missing", "subscription.active", "active", "", time.Time{}, true, false},
		{"a cancellation", "subscription.cancelled", "cancelled", `null`, time.Time{}, true, false},
	}
	c := testClient(t, sent)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			event, err := c.Normalize(corebilling.Delivery{
				DeliveredAt: sent,
				EventType:   tc.eventType,
				RawPayload:  subscriptionDelivery(tc.eventType, tc.status, tc.field),
			})
			if err != nil {
				t.Fatalf("Normalize: %v — a grace deadline must never fail the delivery", err)
			}
			if !event.GracePeriodEndsAt.Equal(tc.want) || event.GracePeriodEndsAtKnown != tc.known {
				t.Errorf("grace deadline = (%s, known %v), want (%s, known %v)",
					event.GracePeriodEndsAt, event.GracePeriodEndsAtKnown, tc.want, tc.known)
			}
			if got := event.GracePeriodEndsAtIssue != ""; got != tc.issue {
				t.Errorf("issue = %q, want one: %v", event.GracePeriodEndsAtIssue, tc.issue)
			}
		})
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
		"past_due":  {corebilling.SubStatusPastDue, true}, // Dodo's grace period
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

func TestNewRejectsAnUnknownEnvironment(t *testing.T) {
	if _, err := New(Config{APIKey: "sk", Environment: "staging"}); err == nil {
		t.Fatal("an unknown environment was accepted; it must fail startup")
	}
	// No key at all is the self-hosted shape, not an error.
	c, err := New(Config{})
	if err != nil || c != nil {
		t.Fatalf("New with no key = (%v, %v), want (nil, nil)", c, err)
	}
}

// A secret pug cannot key an HMAC with must fail startup, not mount a route that
// rejects every real delivery.
func TestNewRejectsAnUnusableWebhookSecret(t *testing.T) {
	for _, tc := range []struct {
		name   string
		secret string
		want   error
	}{
		{"blank after trimming", "   ", ErrEmptySecret},
		{"prefix over non-base64", secretPrefix + "not!base64!", ErrMalformedSecret},
		{"prefix over nothing", secretPrefix, ErrMalformedSecret},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newVerifier(tc.secret)
			if !errors.Is(err, tc.want) {
				t.Errorf("newVerifier(%q) err = %v, want %v", tc.secret, err, tc.want)
			}
		})
	}

	// Unprefixed keys the HMAC verbatim rather than decoding to 24 wrong bytes.
	if _, err := newVerifier(string(secretKey)); err != nil {
		t.Errorf("newVerifier on an unprefixed secret: %v", err)
	}

	// Through New, where a mistyped deploy variable actually lands.
	if _, err := New(Config{APIKey: "sk_test", WebhookSecret: secretPrefix + "not!base64!"}); !errors.Is(err, ErrMalformedSecret) {
		t.Errorf("New err = %v, want ErrMalformedSecret", err)
	}
}

// The branch that decides where real money goes.
func TestNewAcceptsLiveMode(t *testing.T) {
	c, err := New(Config{APIKey: "sk_live", Environment: EnvironmentLive})
	if err != nil {
		t.Fatalf("New in live mode: %v", err)
	}
	if c == nil {
		t.Fatal("New in live mode returned no client")
	}
}

// Parsed before the signature is checked, so it must fail on its own rather than
// read as the epoch.
func TestVerifyRejectsAMalformedTimestamp(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	c := testClient(t, now)
	body := []byte(activeBody)

	headers := signed("evt_1", now, body)
	headers.Set(headerWebhookTimestamp, "not-a-number")
	if _, err := c.Verify(headers, body); !errors.Is(err, ErrTimestamp) {
		t.Errorf("err = %v, want ErrTimestamp", err)
	}
}

// The one shape the inbox retries: the payload changed under us, and a redeploy
// inside the retry window fixes it.
func TestVerifyReportsAnUndecodableBodyAsUndecodable(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	c := testClient(t, now)
	body := []byte(`{"type":`)

	_, err := c.Verify(signed("evt_1", now, body), body)
	if !errors.Is(err, corebilling.ErrUndecodable) {
		t.Errorf("err = %v, want ErrUndecodable", err)
	}
}

// Never a zero event: that stores as cleanly ignored.
func TestNormalizeRefusesASubscriptionItCannotRead(t *testing.T) {
	c := testClient(t, time.Now())
	for _, tc := range []struct {
		name string
		body string
	}{
		{"envelope is not json", `{"type":"subscription.active","data":`},
		{"data is not an object", `{"type":"subscription.active","data":"a string"}`},
		{"no subscription id", `{"type":"subscription.active","data":{"status":"active"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event, err := c.Normalize(corebilling.Delivery{
				EventType:  "subscription.active",
				RawPayload: []byte(tc.body),
			})
			if err == nil {
				t.Fatalf("Normalize accepted %s and returned %+v", tc.name, event)
			}
		})
	}
}

// The flat field is json:"-", so this fallback is the only thing filling
// ProviderCustomerID on the webhook path.
func TestNormalizeTakesTheCustomerFromTheNestedObject(t *testing.T) {
	c := testClient(t, time.Now())
	body := `{"type":"subscription.active","data":{` +
		`"subscription_id":"sub_1","status":"active","customer":{"customer_id":"cus_1"}}}`
	event, err := c.Normalize(corebilling.Delivery{
		EventType:  "subscription.active",
		RawPayload: []byte(body),
	})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if event.ProviderCustomerID != "cus_1" {
		t.Errorf("provider_customer_id = %q, want cus_1", event.ProviderCustomerID)
	}
}

// A non-string VALUE inside metadata does not fail the delivery; metadata that is
// not an object at all does.
func TestNormalizeRefusesMetadataThatIsNotAnObject(t *testing.T) {
	c := testClient(t, time.Now())
	body := `{"type":"subscription.active","data":{` +
		`"subscription_id":"sub_1","status":"active","metadata":"nope"}}`
	if _, err := c.Normalize(corebilling.Delivery{
		EventType:  "subscription.active",
		RawPayload: []byte(body),
	}); err == nil {
		t.Fatal("metadata that is not an object was accepted")
	}
}
