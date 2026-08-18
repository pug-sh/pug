package postgres

import (
	"testing"
	"time"
)

// pgtype reads Y/M/D off the value's own location, so a non-UTC instant would
// store a different day than it names — one whole usage cell off.
func TestNewDateNormalizesToUTC(t *testing.T) {
	kolkata := time.FixedZone("IST", int(5.5*3600))
	// 2026-06-11 02:00 IST is still 2026-06-10 in UTC.
	got := NewDate(time.Date(2026, 6, 11, 2, 0, 0, 0, kolkata))

	if !got.Valid {
		t.Fatal("Valid=false")
	}
	if want := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC); !got.Time.Equal(want) {
		t.Errorf("got %s, want %s", got.Time, want)
	}
	if _, off := got.Time.Zone(); off != 0 {
		t.Errorf("zone offset = %d, want 0: pgtype would encode the local calendar day", off)
	}
}

func TestNewNullableText(t *testing.T) {
	if got := NewNullableText(nil); got.Valid {
		t.Errorf("nil → Valid=true, want false (SQL NULL so coalesce preserves)")
	}

	empty := ""
	if got := NewNullableText(&empty); !got.Valid || got.String != "" {
		t.Errorf("&\"\" → %+v, want {String:\"\", Valid:true} (write empty, e.g. UTC reset)", got)
	}

	val := "Asia/Kolkata"
	if got := NewNullableText(&val); !got.Valid || got.String != val {
		t.Errorf("&%q → %+v, want {String:%q, Valid:true}", val, got, val)
	}
}

func TestOptionalIntsTreatZeroAsAbsent(t *testing.T) {
	if got := NewOptionalInt2(0); got.Valid {
		t.Errorf("0 → Valid=true, want false (SQL NULL: the column's check excludes 0)")
	}
	if got := NewOptionalInt2(17); !got.Valid || got.Int16 != 17 {
		t.Errorf("17 → %+v, want {Int16:17, Valid:true}", got)
	}

	if got := NewOptionalInt8(0); got.Valid {
		t.Errorf("0 → Valid=true, want false (SQL NULL)")
	}
	if got := NewOptionalInt8(5_000_000); !got.Valid || got.Int64 != 5_000_000 {
		t.Errorf("5000000 → %+v, want {Int64:5000000, Valid:true}", got)
	}
}

func TestInt2ToIntInvertsNewOptionalInt2(t *testing.T) {
	if got := Int2ToInt(NewOptionalInt2(0)); got != 0 {
		t.Errorf("NULL → %d, want 0", got)
	}
	if got := Int2ToInt(NewOptionalInt2(31)); got != 31 {
		t.Errorf("31 → %d, want 31", got)
	}
}

// int→int16 narrowing is silent, so a value past the column's range lands inside
// it: callers must range-check before this, and today SetPlan does.
func TestNewOptionalInt2NarrowsSilently(t *testing.T) {
	if got := NewOptionalInt2(65537); got.Int16 != 1 {
		t.Errorf("65537 → Int16=%d, want 1", got.Int16)
	}
}
