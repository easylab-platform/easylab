package repoext

import "testing"

func TestCatalogResolvesEnZh(t *testing.T) {
	en := tr.T("en", MsgMissingPathArgument, nil)
	if en == "" {
		t.Fatal("en empty")
	}
	zh := tr.T("zh", MsgMissingPathArgument, nil)
	if zh == en {
		t.Fatalf("zh did not differ from en (%q)", en)
	}
	if got := tr.T("de", MsgMissingPathArgument, nil); got != en {
		t.Errorf("de fallback = %q, want %q", got, en)
	}
}

func TestCatalogEveryKeyHasEnAndZh(t *testing.T) {
	for k, entry := range catalog {
		if entry["en"] == "" {
			t.Errorf("%s: missing en", k)
		}
		if entry["zh"] == "" {
			t.Errorf("%s: missing zh", k)
		}
	}
}
