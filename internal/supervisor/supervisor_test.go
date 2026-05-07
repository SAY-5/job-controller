package supervisor

import "testing"

func TestArgOrPicksProvidedValue(t *testing.T) {
	got := argOr([]string{"--foo", "bar", "--limit", "42"}, "--limit", "0")
	if got != "42" {
		t.Fatalf("argOr = %q", got)
	}
}

func TestArgOrFallsBackToDefault(t *testing.T) {
	got := argOr([]string{"--foo", "bar"}, "--limit", "999")
	if got != "999" {
		t.Fatalf("argOr = %q", got)
	}
}

func TestArgOrIgnoresFlagWithNoValue(t *testing.T) {
	// Trailing flag with no value: must not crash, must fall through to default.
	got := argOr([]string{"--limit"}, "--limit", "7")
	if got != "7" {
		t.Fatalf("argOr = %q", got)
	}
}
