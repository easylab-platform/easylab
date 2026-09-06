package naming

import "testing"

func TestValidComponent(t *testing.T) {
	valid := []string{"a", "acme", "my.repo", "v1.2", "feature-x", "A_b", "feature/a", "feat/a/b"}
	for _, s := range valid {
		if !ValidComponent(s) {
			t.Errorf("ValidComponent(%q) = false, want true", s)
		}
	}
	invalid := []string{"", ":x", "a:b", "a..b", "a.", "a.lock", ".start", "-start", "sp ace", "/a", "a/", "a//b"}
	for _, s := range invalid {
		if ValidComponent(s) {
			t.Errorf("ValidComponent(%q) = true, want false", s)
		}
	}
}

func TestSessionRoundTrip(t *testing.T) {
	o, r, b, ok := Parse(Session("acme", "my.repo", "feature-x"))
	if !ok || o != "acme" || r != "my.repo" || b != "feature-x" {
		t.Fatalf("round trip = %q %q %q %v", o, r, b, ok)
	}
	if _, _, _, ok := Parse("no-separators"); ok {
		t.Fatal("Parse accepted a name without separators")
	}
}

func TestRequireNamesOffender(t *testing.T) {
	if off, ok := Require([2]string{"org", "ok"}, [2]string{"repo", "bad..name"}); ok || off != "repo" {
		t.Fatalf("Require = %q %v, want repo false", off, ok)
	}
}
