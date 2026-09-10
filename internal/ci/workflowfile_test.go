package ci

import "testing"

const sampleFile = `
version: 1
workflows:
  - name: publish-npm
    preset: npm-publish
  - name: build-image
    preset: container-build
    args: { tag: team/app:main }
  - name: custom
    on: [manual]
    jobs:
      - id: build
        needs: []
        runs_on: [os=linux, is_container=true]
        produce: { action: oci-build, tag: team/app }
      - id: publish
        needs: [build]
        runs_on: [os=linux, is_container=true]
        produce: { action: publish-protocol, protocol: cargo }
`

func TestParseAllWorkflows(t *testing.T) {
	wfs, missing, err := ParseWorkflowFile([]byte(sampleFile), "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("missing = %v", missing)
	}
	if len(wfs) != 3 {
		t.Fatalf("workflows = %d, want 3", len(wfs))
	}
	// preset expansion
	if wfs[0].Jobs[0].Produce.Protocol != "npm" {
		t.Fatalf("npm job = %+v", wfs[0].Jobs[0])
	}
	if wfs[1].Jobs[0].Produce.Tag != "team/app:main" {
		t.Fatalf("build job = %+v", wfs[1].Jobs[0])
	}
	// explicit jobs + DAG
	if len(wfs[2].Jobs) != 2 || wfs[2].Jobs[1].Needs[0] != "build" {
		t.Fatalf("custom = %+v", wfs[2].Jobs)
	}
	// default on = manual
	if len(wfs[0].On) != 1 || wfs[0].On[0] != "manual" {
		t.Fatalf("on = %v", wfs[0].On)
	}
	// org/repo/branch intentionally empty (filled by caller from location)
	if wfs[0].Org != "" || wfs[0].Repo != "" || wfs[0].Branch != "" {
		t.Fatalf("location fields should be empty: %+v", wfs[0])
	}
}

func TestParseSelectByName(t *testing.T) {
	wfs, missing, err := ParseWorkflowFile([]byte(sampleFile), "build-image")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(wfs) != 1 || wfs[0].Name != "build-image" {
		t.Fatalf("wfs = %+v missing=%v", wfs, missing)
	}
	_, missing, err = ParseWorkflowFile([]byte(sampleFile), "nope")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(missing) != 1 || missing[0] != "nope" {
		t.Fatalf("missing = %v", missing)
	}
}

func TestParseRejectsPresetAndJobs(t *testing.T) {
	bad := `
workflows:
  - name: bad
    preset: npm-publish
    jobs: [{ id: x }]
`
	if _, _, err := ParseWorkflowFile([]byte(bad), ""); err == nil {
		t.Fatal("preset + jobs must be rejected")
	}
}

func TestParseRejectsEmpty(t *testing.T) {
	if _, _, err := ParseWorkflowFile([]byte("version: 1\n"), ""); err == nil {
		t.Fatal("empty workflows must be rejected")
	}
}
