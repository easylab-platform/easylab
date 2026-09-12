package ci

import "testing"

func TestPublishBuildArgsExplicit(t *testing.T) {
	job := &Job{Produce: Produce{Action: ActionPublishProtocol, Protocol: "maven", Name: "com.acme", Version: "1.2.3"}}
	args := publishBuildArgsMap(job, "http://easylab", "tok")
	for k, want := range map[string]string{
		"NAME": "com.acme", "VERSION": "1.2.3",
		"ARTIFACT_URL": "http://easylab", "ARTIFACT_TOKEN": "tok",
	} {
		if args[k] != want {
			t.Errorf("build-arg %s = %q, want %q (all: %v)", k, args[k], want, args)
		}
	}
}

func TestPublishBuildArgsNPMRCLine(t *testing.T) {
	job := &Job{Produce: Produce{Action: ActionPublishProtocol, Protocol: "npm"}}
	args := publishBuildArgsMap(job, "http://easylab:8080", "tok")
	if args["NPMRC_LINE"] != "//easylab:8080/pkgs/npm/:_authToken=tok" {
		t.Fatalf("npmrc line wrong: %v", args)
	}
}
