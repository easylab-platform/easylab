package k8s

import "testing"

func TestResolveRuntimeDefaults(t *testing.T) {
	c := newClient(nil, Config{RegistryHost: "easylab.temp.svc.cluster.local:80"})

	linux, err := c.ResolveRuntime("")
	if err != nil {
		t.Fatalf("linux: %v", err)
	}
	if !linux.Derived {
		t.Fatal("linux must be a derived profile")
	}

	win, err := c.ResolveRuntime("windows")
	if err != nil {
		t.Fatalf("windows: %v", err)
	}
	if win.Derived || !win.NeedsTun || win.DeviceLimits["squat.ai/kvm"] != "1" {
		t.Fatalf("windows profile not VM-shaped: %+v", win)
	}
	if win.ReadyTimeoutDuration().Minutes() != 15 {
		t.Fatalf("windows ready timeout = %v", win.ReadyTimeoutDuration())
	}

	if _, err := c.ResolveRuntime("solaris"); err == nil {
		t.Fatal("unknown runtime must error")
	}
}

func TestQualifyRuntimeImage(t *testing.T) {
	c := newClient(nil, Config{RegistryHost: "reg:80"})
	cases := []struct{ in, want string }{
		{"easylab/vm-images/x:v1", "reg:80/easylab/vm-images/x:v1"},
		{"reg:80/easylab/vm-images/x:v1", "reg:80/easylab/vm-images/x:v1"},
		{"docker.io/library/x:v1", "docker.io/library/x:v1"},
	}
	for _, tc := range cases {
		if got := c.qualifyRuntimeImage(tc.in); got != tc.want {
			t.Errorf("qualify(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRuntimeOverrideMerges(t *testing.T) {
	t.Setenv("EASYLAB_SANDBOX_RUNTIMES", `{"windows":{"image":"reg:80/custom/win:v2","readyTimeout":"5m","env":{"CPU_CORES":"4"}}}`)
	c := newClient(nil, Config{RegistryHost: "reg:80"})
	win, err := c.ResolveRuntime("windows")
	if err != nil {
		t.Fatal(err)
	}
	if win.Image != "reg:80/custom/win:v2" {
		t.Fatalf("image override = %q", win.Image)
	}
	if win.ReadyTimeoutDuration().Minutes() != 5 {
		t.Fatalf("timeout override = %v", win.ReadyTimeoutDuration())
	}
	if win.Env["CPU_CORES"] != "4" || win.Env["VERSION"] != "win11" {
		t.Fatalf("env merge = %+v", win.Env)
	}
	if !win.NeedsTun || win.DeviceLimits["squat.ai/kvm"] != "1" {
		t.Fatalf("built-in VM fields lost: %+v", win)
	}
}
