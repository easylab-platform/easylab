package ops

import "testing"

func TestNamespaces(t *testing.T) {
	reg := NewNamespaceRegistry([]string{"staging", "prod", "staging"})
	if reg.Default() != "staging" {
		t.Fatalf("default: %s", reg.Default())
	}
	if !reg.Approved("prod") {
		t.Fatal("prod not approved")
	}
	if reg.Approved("nope") {
		t.Fatal("nope should not be approved")
	}
	if ns, ok := reg.Resolve(""); !ok || ns != "staging" {
		t.Fatalf("resolve empty: %s %v", ns, ok)
	}
	if _, ok := reg.Resolve("nope"); ok {
		t.Fatal("resolve nope should fail")
	}
	// Empty -> default only.
	empty := NewNamespaceRegistry(nil)
	if empty.Default() != "default" {
		t.Fatalf("empty default: %s", empty.Default())
	}
}
