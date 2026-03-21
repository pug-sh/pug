package slogx

import (
	"errors"
	"testing"
)

func TestError(t *testing.T) {
	err := errors.New("something broke")
	attr := Error(err)

	if attr.Key != "error" {
		t.Errorf("Error() key = %q, want %q", attr.Key, "error")
	}
	if attr.Value.Any() != err {
		t.Errorf("Error() value = %v, want %v", attr.Value.Any(), err)
	}
}

func TestErrorNil(t *testing.T) {
	attr := Error(nil)
	if attr.Key != "error" {
		t.Errorf("Error(nil) key = %q, want %q", attr.Key, "error")
	}
	if attr.Value.Any() != nil {
		t.Errorf("Error(nil) value = %v, want nil", attr.Value.Any())
	}
}

func TestRedacted(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value any
		want  string
	}{
		{"nil value", "token", nil, "[EMPTY]"},
		{"empty string", "api_key", "", "[EMPTY]"},
		{"non-empty string", "api_key", "secret-123", "[REDACTED]"},
		{"integer", "count", 42, "[REDACTED]"},
		{"bool", "flag", true, "[REDACTED]"},
		{"struct", "data", struct{}{}, "[REDACTED]"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attr := Redacted(tt.key, tt.value)
			if attr.Key != tt.key {
				t.Errorf("Redacted() key = %q, want %q", attr.Key, tt.key)
			}
			got := attr.Value.String()
			if got != tt.want {
				t.Errorf("Redacted(%q, %v) = %q, want %q", tt.key, tt.value, got, tt.want)
			}
		})
	}
}
