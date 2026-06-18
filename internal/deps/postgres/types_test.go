package postgres

import "testing"

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
