package ci

import (
	"strings"
	"testing"
)

func TestPresetsCoverAllProtocols(t *testing.T) {
	got := Presets()
	for _, p := range publishProtocols {
		want := p + "-publish"
		found := false
		for _, g := range got {
			if g == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing preset %s", want)
		}
	}
	found := false
	for _, g := range got {
		if g == "container-build" {
			found = true
		}
	}
	if !found {
		t.Error("missing container-build preset")
	}
}

func TestExpandManifestDrivenPreset(t *testing.T) {
	job, err := ExpandPreset("npm-publish", "publish", nil)
	if err != nil {
		t.Fatalf("npm-publish: %v", err)
	}
	if job.Produce.Action != ActionPublishProtocol || job.Produce.Protocol != "npm" {
		t.Fatalf("job = %+v", job.Produce)
	}
	if job.ID != "publish" {
		t.Fatalf("id = %q", job.ID)
	}
}

func TestExpandExplicitArgPreset(t *testing.T) {
	// maven requires name+version.
	if _, err := ExpandPreset("maven-publish", "", nil); err == nil {
		t.Fatal("maven-publish without args should error")
	}
	job, err := ExpandPreset("maven-publish", "", map[string]string{"name": "com.acme", "version": "1.0.0"})
	if err != nil {
		t.Fatalf("maven-publish: %v", err)
	}
	if job.Produce.Name != "com.acme" || job.Produce.Version != "1.0.0" {
		t.Fatalf("job = %+v", job.Produce)
	}
}

func TestExpandContainerBuild(t *testing.T) {
	if _, err := ExpandPreset("container-build", "", nil); err == nil {
		t.Fatal("container-build without tag should error")
	}
	job, err := ExpandPreset("container-build", "", map[string]string{"tag": "team/app:main"})
	if err != nil {
		t.Fatalf("container-build: %v", err)
	}
	if job.Produce.Action != ActionOCIBuild || job.Produce.Tag != "team/app:main" {
		t.Fatalf("job = %+v", job.Produce)
	}
}

func TestExpandUnknownPreset(t *testing.T) {
	if _, err := ExpandPreset("nope", "", nil); err == nil || !strings.Contains(err.Error(), "unknown preset") {
		t.Fatalf("err = %v", err)
	}
}
