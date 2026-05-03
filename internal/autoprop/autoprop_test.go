package autoprop

import "testing"

func TestPropertyValue(t *testing.T) {
	tests := []struct {
		key      string
		value    string
		wantKind string
	}{
		{PropMobile, "true", "bool"},
		{PropVerifiedBot, "false", "bool"},
		{PropBotScore, "99", "int"},
		{PropScreenWidth, "390", "int"},
		{PropLatitude, "37.7749", "double"},
		{PropLongitude, "-122.4194", "double"},
		{"$country", "US", "string"},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			got := PropertyValue(tt.key, tt.value)
			switch tt.wantKind {
			case "bool":
				if got.GetBoolValue() != (tt.value == "true") {
					t.Fatalf("expected bool value for %s, got %#v", tt.key, got)
				}
			case "int":
				if got.GetIntValue() == 0 && tt.value != "0" {
					t.Fatalf("expected int value for %s, got %#v", tt.key, got)
				}
			case "double":
				if got.GetDoubleValue() == 0 {
					t.Fatalf("expected double value for %s, got %#v", tt.key, got)
				}
			case "string":
				if got.GetStringValue() != tt.value {
					t.Fatalf("expected string value %q for %s, got %#v", tt.value, tt.key, got)
				}
			}
		})
	}
}
