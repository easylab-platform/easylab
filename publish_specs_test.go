package main

import (
	"sort"
	"testing"
)

func TestPublishSpecsExport(t *testing.T) {
	if len(publishSpecs) != 14 {
		t.Fatalf("publishSpecs has %d protocols, want 14", len(publishSpecs))
	}
	manifestDriven := []string{"npm", "pypi", "cargo", "rubygems", "conan", "pub", "helm", "nuget"}
	for _, p := range manifestDriven {
		spec, ok := publishSpecs[p]
		if !ok {
			t.Fatalf("missing protocol %s", p)
		}
		if len(spec.required) != 0 {
			t.Errorf("%s should be manifest-driven (no required args), got %v", p, spec.required)
		}
	}
	if spec := publishSpecs["generic"]; len(spec.required) != 3 {
		t.Errorf("generic required = %v, want [NAME VERSION FILE]", spec.required)
	}
	for proto, spec := range publishSpecs {
		if spec.image == "" && !spec.multi {
			t.Errorf("%s: single-stage spec needs an image", proto)
		}
	}
	// sorted protocol list for the specs endpoint
	names := make([]string, 0, len(publishSpecs))
	for p := range publishSpecs {
		names = append(names, p)
	}
	sort.Strings(names)
}
