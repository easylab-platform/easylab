package k8s

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// RuntimeProfile is a sandbox execution profile. The linux profile derives an
// image (base + injected worker); VM profiles (windows/macos) use a prebuilt
// worker image easylab serves and add the device/security context a VM needs.
//
// Profiles are supplied by the deployment (chart values -> EASYLAB_SANDBOX_
// RUNTIMES JSON) with built-in defaults; a request only names the runtime.
type RuntimeProfile struct {
	Name string `json:"-"`

	// Image is the worker image. For Derived profiles it is unused and the
	// image is built per base image; otherwise it is used verbatim.
	Image string `json:"image"`
	// Derived: build base+worker on demand (linux). False: use Image as-is.
	Derived bool `json:"derived"`

	// WorkerPort is the port the worker listens on inside the sandbox.
	WorkerPort int32 `json:"workerPort"`

	NeedsTun         bool                         `json:"needsTun"`
	NodeSelector     map[string]string            `json:"nodeSelector"`
	DeviceLimits     map[string]string            `json:"deviceLimits"`
	ImagePullSecrets []string                     `json:"imagePullSecrets"`
	SecurityContext  *corev1.SecurityContext      `json:"securityContext"`
	Env              map[string]string            `json:"env"`
	Resources        *corev1.ResourceRequirements `json:"resources"`

	// ReadyTimeout bounds how long LaunchSandbox waits for the worker health
	// endpoint. Empty defaults to 60s (VMs need minutes).
	ReadyTimeout string `json:"readyTimeout"`
}

// ReadyTimeoutDuration parses ReadyTimeout (0 => 60s default).
func (p RuntimeProfile) ReadyTimeoutDuration() time.Duration {
	if p.ReadyTimeout == "" {
		return 60 * time.Second
	}
	d, err := time.ParseDuration(p.ReadyTimeout)
	if err != nil || d <= 0 {
		return 60 * time.Second
	}
	return d
}

// Port returns the worker port (48080 default).
func (p RuntimeProfile) Port() int32 {
	if p.WorkerPort != 0 {
		return p.WorkerPort
	}
	return 48080
}

// defaultRuntimes are the built-in profiles; the deployment may override any
// field via EASYLAB_SANDBOX_RUNTIMES.
func defaultRuntimes() map[string]RuntimeProfile {
	return map[string]RuntimeProfile{
		"linux": {
			Name:         "linux",
			Derived:      true,
			WorkerPort:   48080,
			ReadyTimeout: "60s",
		},
		"windows": {
			Name:       "windows",
			Derived:    false,
			Image:      "easylab/vm-images/easyworker-windows:v1.2.0",
			WorkerPort: 48080,
			NeedsTun:     true,
			DeviceLimits: map[string]string{"squat.ai/kvm": "1"},
			SecurityContext: &corev1.SecurityContext{
				RunAsUser:                int64Ptr(0),
				AllowPrivilegeEscalation: boolPtr(true),
				Privileged:               boolPtr(false),
				Capabilities: &corev1.Capabilities{Add: []corev1.Capability{
					"NET_ADMIN", "NET_RAW", "SYS_ADMIN", "SYS_NICE", "MKNOD",
					"CHOWN", "SETUID", "SETGID", "DAC_OVERRIDE", "FOWNER", "KILL",
				}},
				AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined},
				SeccompProfile:  &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
			},
			Resources: &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("8Gi")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("8Gi")},
			},
			Env: map[string]string{
				"VERSION": "win11", "EDITION": "pro", "DISK_FMT": "qcow2",
				"MANUAL": "N", "SAMBA": "N",
				"CPU_CORES": "2", "RAM_SIZE": "8192M", "DISK_SIZE": "128G",
			},
			ReadyTimeout: "15m",
		},
		"macos": {
			Name:       "macos",
			Derived:    false,
			Image:      "easylab/vm-images/easyworker-macos:v1.2.0",
			WorkerPort: 48080,
			NeedsTun:     true,
			DeviceLimits: map[string]string{"squat.ai/kvm": "1"},
			SecurityContext: &corev1.SecurityContext{
				RunAsUser:                int64Ptr(0),
				AllowPrivilegeEscalation: boolPtr(true),
				Privileged:               boolPtr(false),
				Capabilities: &corev1.Capabilities{Add: []corev1.Capability{
					"NET_ADMIN", "NET_RAW", "SYS_ADMIN", "SYS_NICE", "MKNOD",
					"CHOWN", "SETUID", "SETGID", "DAC_OVERRIDE", "FOWNER", "KILL",
				}},
				AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined},
				SeccompProfile:  &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
			},
			Resources: &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("8Gi")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("8Gi")},
			},
			Env: map[string]string{
				"VERSION": "14", "MANUAL": "N",
				"CPU_CORES": "2", "RAM_SIZE": "8192M", "DISK_SIZE": "128G",
			},
			ReadyTimeout: "15m",
		},
	}
}

// loadRuntimes merges built-in defaults with EASYLAB_SANDBOX_RUNTIMES (a JSON
// object of runtime name -> partial profile). Unknown runtimes are allowed; a
// request naming one must supply an image in the override.
func loadRuntimes() map[string]RuntimeProfile {
	out := defaultRuntimes()
	raw := strings.TrimSpace(os.Getenv("EASYLAB_SANDBOX_RUNTIMES"))
	if raw == "" {
		return out
	}
	var override map[string]RuntimeProfile
	if err := json.Unmarshal([]byte(raw), &override); err != nil {
		// A malformed override must not silently drop the VM profiles; keep
		// the defaults and let the deployment surface the parse error via logs.
		return out
	}
	for name, p := range override {
		base, ok := out[name]
		if !ok {
			base = RuntimeProfile{Name: name, Derived: false, WorkerPort: 48080}
		}
		base.Name = name
		mergeProfile(&base, p)
		out[name] = base
	}
	return out
}

// mergeProfile overlays non-zero fields of src onto dst.
func mergeProfile(dst *RuntimeProfile, src RuntimeProfile) {
	if src.Image != "" {
		dst.Image = src.Image
	}
	// Derived is meaningful only when explicitly present; a VM override leaves
	// it false (its zero value), which matches the intended "use Image" mode.
	if src.Image != "" {
		dst.Derived = src.Derived
	}
	if src.WorkerPort != 0 {
		dst.WorkerPort = src.WorkerPort
	}
	if src.NeedsTun {
		dst.NeedsTun = true
	}
	if src.NodeSelector != nil {
		dst.NodeSelector = src.NodeSelector
	}
	if src.DeviceLimits != nil {
		dst.DeviceLimits = src.DeviceLimits
	}
	if src.ImagePullSecrets != nil {
		dst.ImagePullSecrets = src.ImagePullSecrets
	}
	if src.SecurityContext != nil {
		dst.SecurityContext = src.SecurityContext
	}
	if src.Env != nil {
		if dst.Env == nil {
			dst.Env = map[string]string{}
		}
		for k, v := range src.Env {
			dst.Env[k] = v
		}
	}
	if src.Resources != nil {
		dst.Resources = src.Resources
	}
	if src.ReadyTimeout != "" {
		dst.ReadyTimeout = src.ReadyTimeout
	}
}

// RuntimeNames lists the configured runtime names (sorted), for diagnostics.
func (c *Client) RuntimeNames() []string {
	names := make([]string, 0, len(c.runtimes))
	for n := range c.runtimes {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ResolveRuntime returns the profile for a request runtime ("" -> linux).
// A VM image without an explicit registry host is qualified with this
// deployment's registry host so the deployment ports without hardcoding it.
func (c *Client) ResolveRuntime(name string) (RuntimeProfile, error) {
	if name == "" {
		name = "linux"
	}
	p, ok := c.runtimes[name]
	if !ok {
		return RuntimeProfile{}, fmt.Errorf("unknown sandbox runtime %q", name)
	}
	if !p.Derived {
		if p.Image == "" {
			return RuntimeProfile{}, fmt.Errorf("sandbox runtime %q has no image configured", name)
		}
		p.Image = c.qualifyRuntimeImage(p.Image)
	}
	return p, nil
}

// qualifyRuntimeImage prefixes an image ref that has no registry host with this
// client's registry host (e.g. "easylab/vm-images/x:tag" ->
// "<host>/easylab/vm-images/x:tag").
func (c *Client) qualifyRuntimeImage(ref string) string {
	first := ref
	if i := strings.IndexByte(ref, '/'); i >= 0 {
		first = ref[:i]
	}
	if strings.Contains(first, ".") || strings.Contains(first, ":") || first == "localhost" {
		return ref // already has a registry host
	}
	if c.registryHost == "" {
		return ref
	}
	return c.registryHost + "/" + ref
}

func int64Ptr(v int64) *int64 { return &v }
