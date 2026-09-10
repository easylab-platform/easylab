package ci

import "testing"

func TestPublishBuildArgsExplicit(t *testing.T) {
	job := &Job{Produce: Produce{Action: ActionPublishProtocol, Protocol: "maven", Name: "com.acme", Version: "1.2.3"}}
	args := publishBuildArgs(job, "http://easylab", "tok")
	joined := map[string]bool{}
	for _, a := range args {
		joined[a] = true
	}
	for _, want := range []string{"NAME=com.acme", "VERSION=1.2.3", "ARTIFACT_URL=http://easylab", "ARTIFACT_TOKEN=tok"} {
		if !joined[want] {
			t.Errorf("missing build-arg %q in %v", want, args)
		}
	}
}

func TestPublishBuildArgsNPMRCLine(t *testing.T) {
	job := &Job{Produce: Produce{Action: ActionPublishProtocol, Protocol: "npm"}}
	args := publishBuildArgs(job, "http://easylab:8080", "tok")
	found := false
	for _, a := range args {
		if a == "NPMRC_LINE=//easylab:8080/pkgs/npm/:_authToken=tok" {
			found = true
		}
	}
	if !found {
		t.Fatalf("npmrc line missing: %v", args)
	}
}
