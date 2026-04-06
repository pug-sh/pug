package profiles

import (
	"testing"
	"time"
)

func TestProfileListCursorRoundTrip(t *testing.T) {
	original := &profileListCursor{
		CreateTime: time.Date(2025, 6, 15, 10, 30, 0, 0, time.UTC),
		ID:         "abc123def456ghi789jk",
	}

	token, err := original.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	decoded, err := decodeProfileListCursor(token)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !decoded.CreateTime.Equal(original.CreateTime) {
		t.Errorf("CreateTime: got %v, want %v", decoded.CreateTime, original.CreateTime)
	}
	if decoded.ID != original.ID {
		t.Errorf("ID: got %q, want %q", decoded.ID, original.ID)
	}
}

func TestDecodeProfileListCursor_Invalid(t *testing.T) {
	tests := []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"not base64", "!!!invalid!!!"},
		{"not json", "bm90LWpzb24"},
		{"missing time", "eyJpIjoiYWJjMTIzIn0"},
		{"missing id", "eyJ0IjoiMjAyNS0wNi0xNVQxMDozMDowMFoifQ"},
		{"zero time with id", "eyJ0IjoiMDAwMS0wMS0wMVQwMDowMDowMFoiLCJpIjoiYWJjIn0"},
		{"valid time empty id", "eyJ0IjoiMjAyNS0wNi0xNVQxMDozMDowMFoiLCJpIjoiIn0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := decodeProfileListCursor(tt.token); err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}
