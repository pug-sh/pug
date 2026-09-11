package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/spf13/cobra"
)

func billingSetCmd(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	cmd, _, err := newBillingCmd().Find([]string{"set"})
	if err != nil {
		t.Fatalf("find set: %v", err)
	}
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return cmd
}

// The most expensive bug this CLI could have: a renewal that quietly reverts a
// customer's negotiated quota to the catalog number.
func TestBillingChangeOmittedFlagsKeepStoredValues(t *testing.T) {
	change, err := billingChange(billingSetCmd(t, "--plan", "custom"))
	if err != nil {
		t.Fatalf("billingChange: %v", err)
	}
	if change.PlanSlug != "custom" {
		t.Fatalf("plan = %q, want custom", change.PlanSlug)
	}
	if change.ContractEndsAt != nil {
		t.Fatalf("until = %v, want nil (keep stored)", *change.ContractEndsAt)
	}
	if change.IncludedEvents != nil {
		t.Fatalf("events = %v, want nil (keep stored)", *change.IncludedEvents)
	}
	if change.RetentionDays != nil {
		t.Fatalf("retention days = %v, want nil (keep stored)", *change.RetentionDays)
	}
	if change.DisplayName != nil {
		t.Fatalf("name = %q, want nil (keep stored)", *change.DisplayName)
	}
	if change.AnchorDay != nil {
		t.Fatalf("anchor day = %v, want nil (keep stored)", *change.AnchorDay)
	}
	if change.Note != nil {
		t.Fatalf("note = %q, want nil (keep stored)", *change.Note)
	}
	if change.FlatFeeCents != nil || change.BlockRateCents != nil {
		t.Fatalf("money = %v/%v, want nil (keep stored)", change.FlatFeeCents, change.BlockRateCents)
	}
}

func TestBillingChangeEmptyValuesClear(t *testing.T) {
	cmd := billingSetCmd(t, "--plan", "free", "--events", "0", "--retention-days", "0",
		"--name", "", "--anchor-day", "0", "--until", "", "--note", "", "--flat-fee", "0", "--block-rate", "0")
	change, err := billingChange(cmd)
	if err != nil {
		t.Fatalf("billingChange: %v", err)
	}
	if change.IncludedEvents == nil || *change.IncludedEvents != 0 {
		t.Fatalf("events = %v, want a pointer to 0 (clear)", change.IncludedEvents)
	}
	if change.RetentionDays == nil || *change.RetentionDays != 0 {
		t.Fatalf("retention days = %v, want a pointer to 0 (clear)", change.RetentionDays)
	}
	if change.DisplayName == nil || *change.DisplayName != "" {
		t.Fatalf("name = %v, want a pointer to \"\" (clear)", change.DisplayName)
	}
	if change.AnchorDay == nil || *change.AnchorDay != 0 {
		t.Fatalf("anchor day = %v, want a pointer to 0 (clear)", change.AnchorDay)
	}
	if change.ContractEndsAt == nil || !change.ContractEndsAt.IsZero() {
		t.Fatalf("contract end = %v, want a pointer to the zero time (clear)", change.ContractEndsAt)
	}
	if change.Note == nil || *change.Note != "" {
		t.Fatalf("note = %v, want a pointer to \"\" (clear)", change.Note)
	}
	if change.FlatFeeCents == nil || *change.FlatFeeCents != 0 || change.BlockRateCents == nil || *change.BlockRateCents != 0 {
		t.Fatalf("money = %v/%v, want pointers to 0 (clear)", change.FlatFeeCents, change.BlockRateCents)
	}
}

// --until names the last day the deal runs; the resolver compares half-open, so
// the stored instant is the following midnight.
func TestBillingChangeUntilIsInclusive(t *testing.T) {
	change, err := billingChange(billingSetCmd(t, "--plan", "custom", "--until", "2026-12-31"))
	if err != nil {
		t.Fatalf("billingChange: %v", err)
	}
	want := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	if change.ContractEndsAt == nil || !change.ContractEndsAt.Equal(want) {
		t.Fatalf("contract end = %v, want %v", change.ContractEndsAt, want)
	}
}

func TestBillingChangeRejectsBadValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"negative events", []string{"--plan", "custom", "--events", "-1"}, "--events"},
		{"partial block", []string{"--plan", "custom", "--events", "250000"}, "--events"},
		{"negative fee", []string{"--plan", "custom", "--flat-fee", "-1"}, "--flat-fee"},
		{"negative rate", []string{"--plan", "custom", "--block-rate", "-1"}, "--block-rate"},
		{"negative retention", []string{"--plan", "custom", "--retention-days", "-1"}, "--retention-days"},
		{"anchor day too high", []string{"--plan", "free", "--anchor-day", "32"}, "--anchor-day"},
		{"anchor day negative", []string{"--plan", "free", "--anchor-day", "-1"}, "--anchor-day"},
		{"until is not a date", []string{"--plan", "custom", "--until", "31/12/2026"}, "--until"},
		{"until carries a time", []string{"--plan", "custom", "--until", "2026-12-31T00:00:00Z"}, "--until"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := billingChange(billingSetCmd(t, tc.args...))
			if err == nil {
				t.Fatalf("expected an error for %v", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to name %s", err, tc.want)
			}
		})
	}
}

// The trial slug has one writer, and it is not `set`.
func TestGrantableSlugsExcludeTrialAndRetired(t *testing.T) {
	got := grantableSlugs()
	if slices.Contains(got, corebilling.SlugTrial) {
		t.Fatalf("slugs = %v, want no %q", got, corebilling.SlugTrial)
	}
	for _, want := range []string{corebilling.SlugFree, corebilling.SlugCustom} {
		if !slices.Contains(got, want) {
			t.Fatalf("slugs = %v, want it to offer %q", got, want)
		}
	}
	for _, c := range corebilling.Cards() {
		if c.Retired && slices.Contains(got, c.Slug) {
			t.Fatalf("slugs = %v, want no retired card %q", got, c.Slug)
		}
		if !c.Retired && !slices.Contains(got, c.Slug) {
			t.Fatalf("slugs = %v, want it to offer %q", got, c.Slug)
		}
	}
}

// Every write is attributed, so none of them may run without an actor.
func TestBillingWritesRequireAnActor(t *testing.T) {
	for _, name := range []string{"set", "extend-trial", "clear", "invoice void", "invoice retry"} {
		cmd, _, err := newBillingCmd().Find(strings.Fields(name))
		if err != nil {
			t.Fatalf("find %s: %v", name, err)
		}
		flag := cmd.Flags().Lookup("actor")
		if flag == nil {
			t.Fatalf("%s has no --actor flag", name)
		}
		if flag.Annotations[cobra.BashCompOneRequiredFlag] == nil {
			t.Fatalf("%s --actor is not required", name)
		}
	}
}
