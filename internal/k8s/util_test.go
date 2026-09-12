package k8s

import "testing"

func TestRewriteImageRef(t *testing.T) {
	host := "easylab.temp.svc.cluster.local:80"
	cases := []struct{ in, want string }{
		{"docker.io/library/nginx:alpine", host + "/docker.io/library/nginx:alpine"},
		{"nginx:alpine", host + "/docker.io/library/nginx:alpine"},
		{"nginx", host + "/docker.io/library/nginx"},
		{"library/nginx:alpine", host + "/docker.io/library/nginx:alpine"},
		{"ghcr.io/foo/bar:v1", host + "/ghcr.io/foo/bar:v1"},
		{"quay.io/foo/bar@sha256:abc", host + "/quay.io/foo/bar@sha256:abc"},
		// Already local / in-cluster: unchanged.
		{host + "/easylab/sandbox:abc", host + "/easylab/sandbox:abc"},
		{"easylab.temp.svc.cluster.local:80/x:y", "easylab.temp.svc.cluster.local:80/x:y"},
		{"some.svc.cluster.local/x:y", "some.svc.cluster.local/x:y"},
		{"localhost:5000/x:y", "localhost:5000/x:y"},
		{"", ""},
	}
	for _, c := range cases {
		if got := RewriteImageRef(c.in, host); got != c.want {
			t.Errorf("RewriteImageRef(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
