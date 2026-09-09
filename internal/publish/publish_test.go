package publish

import (
	"strings"
	"testing"
)

func TestSupported(t *testing.T) {
	got := Supported()
	if len(got) < 10 {
		t.Fatalf("supported protocols = %v (want >=10)", got)
	}
	for _, want := range []string{"npm", "cargo", "pypi", "go", "generic"} {
		found := false
		for _, g := range got {
			if g == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing protocol %q", want)
		}
	}
}

func TestRenderGeneric(t *testing.T) {
	c, err := Render("generic", "myfile", "1.0.0", "dist/app.tar.gz", "http://easylab", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c, "FROM ") || !strings.Contains(c, "ARTIFACT_URL") || !strings.Contains(c, "generic") {
		t.Fatalf("containerfile = %q", c)
	}
}

func TestRenderUnsupported(t *testing.T) {
	if _, err := Render("nope", "", "", "", "", ""); err == nil {
		t.Fatal("expected unsupported error")
	}
}
