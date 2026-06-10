package tzx

import (
	"testing"
	"time"
)

func TestIsUTCAndNormalize(t *testing.T) {
	for _, name := range []string{"", "UTC"} {
		if !IsUTC(name) {
			t.Errorf("IsUTC(%q) = false, want true", name)
		}
		if got := Normalize(name); got != "" {
			t.Errorf("Normalize(%q) = %q, want \"\"", name, got)
		}
	}
	if IsUTC("Asia/Kolkata") {
		t.Error("IsUTC(Asia/Kolkata) = true, want false")
	}
	if got := Normalize("Asia/Kolkata"); got != "Asia/Kolkata" {
		t.Errorf("Normalize(Asia/Kolkata) = %q, want unchanged", got)
	}
}

func TestValidate(t *testing.T) {
	valid := []string{"", "UTC", "Asia/Kolkata", "America/Argentina/Buenos_Aires", "Etc/GMT+5"}
	for _, name := range valid {
		if err := Validate(name); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", name, err)
		}
	}
	invalid := []string{
		"Not/A/Zone",                // well-formed charset but unknown to tzdata
		"Asia/Kolkata'; drop table", // injection attempt — fails charset
		"Asia/Kolkata\"",            // quote — fails charset
		"a much too long timezone name that exceeds the sixty four character cap zzz",
	}
	for _, name := range invalid {
		if err := Validate(name); err == nil {
			t.Errorf("Validate(%q) = nil, want error", name)
		}
	}
}

func TestCoerce(t *testing.T) {
	if got := Coerce("Asia/Kolkata"); got != "Asia/Kolkata" {
		t.Errorf("Coerce(valid) = %q, want Asia/Kolkata", got)
	}
	if got := Coerce("UTC"); got != "" {
		t.Errorf("Coerce(UTC) = %q, want \"\"", got)
	}
	// Malformed/unknown coerces to UTC ("") rather than failing.
	for _, name := range []string{"Not/A/Zone", "evil'); drop"} {
		if got := Coerce(name); got != "" {
			t.Errorf("Coerce(%q) = %q, want \"\" (UTC fallback)", name, got)
		}
	}
}

func TestLoad(t *testing.T) {
	for _, name := range []string{"", "UTC"} {
		loc, err := Load(name)
		if err != nil || loc != time.UTC {
			t.Errorf("Load(%q) = (%v, %v), want (UTC, nil)", name, loc, err)
		}
	}
	loc, err := Load("Asia/Kolkata")
	if err != nil {
		t.Fatalf("Load(Asia/Kolkata) error: %v", err)
	}
	// Asia/Kolkata is UTC+05:30 with no DST — a stable offset to assert on.
	_, offset := time.Date(2026, 6, 10, 0, 0, 0, 0, loc).Zone()
	if offset != int((5*time.Hour+30*time.Minute)/time.Second) {
		t.Errorf("Asia/Kolkata offset = %ds, want 19800s", offset)
	}
	if _, err := Load("Not/A/Zone"); err == nil {
		t.Error("Load(Not/A/Zone) = nil error, want error")
	}
}
