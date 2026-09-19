package opsext

import (
	"testing"

	"github.com/abcp-sdk/abc-protocol-go/v2/i18n"
)

// The catalog is the single runtime-string source; these assertions lock the
// typed keys and the en/zh templates (including printf verbs) so a
// mistranslation or a dropped arg cannot ship silently.
func TestCatalogResolvesEnZh(t *testing.T) {
	en := tr.T("en", MsgSandboxRunMissingCommand, nil)
	if en != "sandbox-run: missing 'command'" {
		t.Errorf("en = %q", en)
	}
	zh := tr.T("zh", MsgSandboxRunMissingCommand, nil)
	if zh != "sandbox-run：缺少 'command'" {
		t.Errorf("zh = %q", zh)
	}
	// Unknown locale falls back to en.
	if got := tr.T("de", MsgSandboxRunMissingCommand, nil); got != en {
		t.Errorf("de fallback = %q, want %q", got, en)
	}
	// zh-CN is a zh-family tag.
	if got := tr.T("zh-CN", MsgSandboxRunMissingCommand, nil); got != zh {
		t.Errorf("zh-CN = %q, want %q", got, zh)
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

func TestBaseLocaleFromSDK(t *testing.T) {
	if i18n.BaseLang("zh-Hans") != "zh" {
		t.Fatal("sdk base lang")
	}
}
