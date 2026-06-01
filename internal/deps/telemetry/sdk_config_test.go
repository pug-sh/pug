package telemetry

import (
	"testing"
)

func TestParseOtelMode(t *testing.T) {
	tests := []struct {
		name    string
		env     string
		want    string
		wantErr bool
	}{
		{name: "unset defaults otlp", env: "", want: "otlp"},
		{name: "otlp explicit", env: "otlp", want: "otlp"},
		{name: "stdout", env: "stdout", want: "stdout"},
		{name: "stdout uppercase", env: "STDOUT", want: "stdout"},
		{name: "invalid local", env: "local", wantErr: true},
		{name: "invalid", env: "file", wantErr: true},
		{name: "typo", env: "stdou", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PUG_OTEL", tt.env)
			got, err := parseOtelMode()
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseOtelMode: %v", err)
			}
			if got != tt.want {
				t.Fatalf("parseOtelMode() = %q, want %q", got, tt.want)
			}
		})
	}
}
