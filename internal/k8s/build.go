package k8s

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BuildOptions describes one image build.
type BuildOptions struct {
	// ContextDir is easylab's in-container build context dir (under /data),
	// holding the repo tree + Dockerfile.
	ContextDir string
	Dockerfile string // relative to ContextDir; default "Dockerfile"
	Image      string // destination ref, pushed to easylab's OCI registry
	BuildArgs  map[string]string
	NoCache    bool
}

// BuildImage builds + pushes an image using an ephemeral, NON-privileged
// rootless buildkit pod. Two containers share a unix-socket emptyDir:
//   - buildkitd (rootless, SYS_ADMIN + Unconfined, same posture as the shared
//     buildkitd) serves the build
//   - buildctl drives the build and pushes to easylab's registry
//
// The pod mounts the shared data root (hostPath), so easylab reads build
// progress straight off <meta>/log and the exit code off <meta>/status —
// no pods/log or pods/exec RBAC needed.
//
// Cache is registry-backed (type=registry,ref=<host>/<cacheRepo>) so it is
// shared across ephemeral build pods and survives pod recreation.
func (c *Client) BuildImage(ctx context.Context, opt BuildOptions, logf func(string)) error {
	if _, err := c.hostDataRoot(); err != nil {
		return err
	}
	id := fmt.Sprintf("b%d", time.Now().UnixNano())
	// Local file I/O uses CONTAINER paths (under /data, the hostPath mount);
	// only the pod spec uses the corresponding HOST paths.
	base := filepath.Join(dataDir(), "buildtmp", id)
	metaDir := filepath.Join(base, "meta")
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		return err
	}
	// The build containers run as uid 1000 (rootless buildkit), while easylab
	// runs as root. hostPath volumes keep the creator's ownership, so open the
	// meta dir so buildctl can write log/status and read config.
	if err := os.Chmod(metaDir, 0o777); err != nil {
		return err
	}
	hostBase := c.hostPath(base)
	hostCtx := c.hostPath(opt.ContextDir)
	filename := opt.Dockerfile
	if filename == "" {
		filename = "Dockerfile"
	}
	if err := c.writeBuildMeta(metaDir, opt, filename); err != nil {
		return err
	}
	logPath := filepath.Join(metaDir, "log")
	statusPath := filepath.Join(metaDir, "status")
	_ = os.Remove(statusPath)

	pod := c.buildPodSpec(id, hostCtx, hostBase, opt)
	if _, err := c.cs.CoreV1().Pods(c.namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create build pod: %w", err)
	}
	defer func() {
		_ = c.cs.CoreV1().Pods(c.namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{})
	}()

	if err := c.tailBuild(ctx, logPath, statusPath, logf); err != nil {
		return err
	}
	if b, err := os.ReadFile(statusPath); err != nil || strings.TrimSpace(string(b)) != "0" {
		return fmt.Errorf("buildkit build failed (exit %s)", strings.TrimSpace(string(b)))
	}
	return nil
}

// writeBuildMeta writes the buildkitd registry config and the buildctl args
// into the shared meta dir (visible to the build pod via hostPath).
func (c *Client) writeBuildMeta(metaDir string, opt BuildOptions, filename string) error {
	auth := base64.StdEncoding.EncodeToString([]byte("agent:" + c.registryToken))
	// Register credentials under both the registry host:port and the bare host,
	// so buildkit matches regardless of how it normalizes the ref (it uses the
	// host of the *challenge realm* for the token request, which may omit :80).
	auths := map[string]map[string]string{c.registryHost: {"auth": auth}}
	if host, _, ok := strings.Cut(c.registryHost, ":"); ok {
		auths[host] = map[string]string{"auth": auth}
	}
	cfg := map[string]any{"auths": auths}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(metaDir, "config.json"), b, 0o644); err != nil {
		return err
	}
	// Trust easylab's own registry over plain HTTP (self-signed/HTTP only).
	// The push token is obtained by buildkit from the /token endpoint using the
	// docker config credentials above; buildkitd.toml has no auth field.
	toml := fmt.Sprintf("[registry.%q]\n  http = true\n  insecure = true\n", c.registryHost)
	if err := os.WriteFile(filepath.Join(metaDir, "buildkitd.toml"), []byte(toml), 0o644); err != nil {
		return err
	}
	args := []string{"--opt", "filename=" + filename}
	for k, v := range opt.BuildArgs {
		args = append(args, "--opt", "build-arg:"+k+"="+v)
	}
	if opt.NoCache {
		args = append(args, "--no-cache")
	}
	// Registry cache: shared across ephemeral pods, survives recreation.
	if ref := c.cacheRefFor(opt.Image); ref != "" {
		args = append(args, "--export-cache", "type=registry,ref="+ref+",mode=max",
			"--import-cache", "type=registry,ref="+ref)
	}
	return os.WriteFile(filepath.Join(metaDir, "buildctl.args"), []byte(joinLines(args)), 0o644)
}

// proxyEnv returns HTTP(S)_PROXY/NO_PROXY env for the build containers so
// base-image/manifest fetches route through the upstream proxy.
func (c *Client) proxyEnv() []corev1.EnvVar {
	if c.proxy == "" {
		return nil
	}
	return []corev1.EnvVar{
		{Name: "HTTP_PROXY", Value: c.proxy},
		{Name: "HTTPS_PROXY", Value: c.proxy},
		{Name: "NO_PROXY", Value: "localhost,127.0.0.1,.svc.cluster.local,.svc"},
	}
}

// cacheRefFor derives the registry cache tag for an image ref
// (<host>/<repo>:<tag> -> <host>/easylab-cache/<repo>:<tag>).
func (c *Client) cacheRefFor(image string) string {
	name := image
	host := ""
	if i := strings.IndexByte(name, '/'); i >= 0 {
		host = name[:i]
	}
	tag := "latest"
	if i := strings.LastIndexByte(name, ':'); i >= 0 && i > strings.LastIndexByte(name, '/') {
		tag = name[i+1:]
		name = name[:i]
	}
	repo := name
	if host != "" {
		repo = name[len(host)+1:]
	}
	return fmt.Sprintf("%s/easylab-cache/%s:%s", c.registryHost, repo, tag)
}

func (c *Client) buildPodSpec(id, hostCtx, hostBase string, opt BuildOptions) *corev1.Pod {
	unconfined := corev1.SeccompProfileTypeUnconfined
	appArmor := corev1.AppArmorProfileTypeUnconfined
	rootless := true
	uid := int64(1000)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "easylab-build-" + id, Namespace: c.namespace, Labels: map[string]string{"easylab/build": "1"}},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			SecurityContext: &corev1.PodSecurityContext{
				SeccompProfile: &corev1.SeccompProfile{Type: unconfined},
			},
			Volumes: []corev1.Volume{
				{Name: "sock", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "ctx", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: hostCtx, Type: hpPtr(corev1.HostPathDirectory)}}},
				{Name: "meta", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: hostBase + "/meta", Type: hpPtr(corev1.HostPathDirectoryOrCreate)}}},
			},
			Containers: []corev1.Container{
				{
					Name:  "buildkitd",
					Image: c.buildkitImage,
					Args: []string{
						"--addr", "unix:///run/buildkit/buildkitd.sock",
						"--config", "/meta/buildkitd.toml",
						// Rootless buildkit: run the OCI worker without an
						// inner process sandbox (the container already bounds
						// the build).
						"--oci-worker-no-process-sandbox",
					},
					SecurityContext: &corev1.SecurityContext{
						RunAsUser:                &uid,
						RunAsNonRoot:             &rootless,
						AllowPrivilegeEscalation: boolPtr(true),
						AppArmorProfile:          &corev1.AppArmorProfile{Type: appArmor},
						Capabilities:             &corev1.Capabilities{Add: []corev1.Capability{"SYS_ADMIN"}},
						SeccompProfile:           &corev1.SeccompProfile{Type: unconfined},
					},
					Env: append([]corev1.EnvVar{
						// Registry auth (push + base-image pulls) happens in the
						// buildkitd daemon, so it must read the docker config.
						{Name: "DOCKER_CONFIG", Value: "/meta"},
					}, c.proxyEnv()...),
					VolumeMounts: []corev1.VolumeMount{
						{Name: "sock", MountPath: "/run/buildkit"},
						{Name: "meta", MountPath: "/meta"},
					},
					Resources: buildResources("1", "2Gi"),
				},
				{
					Name:    "buildctl",
					Image:   c.buildkitImage,
					Command: []string{"/bin/sh", "-c"},
					Args: []string{`set -e
: > /meta/log
buildctl --addr unix:///run/buildkit/buildkitd.sock build \
  --frontend dockerfile.v0 \
  --local context=/ctx \
  --local dockerfile=/ctx \
  $(cat /meta/buildctl.args) \
  --output type=image,name="$IMAGE",push=true \
  --progress plain >>/meta/log 2>&1
echo $? > /meta/status`},
					Env: append([]corev1.EnvVar{
						{Name: "IMAGE", Value: opt.Image},
						{Name: "DOCKER_CONFIG", Value: "/meta"},
					}, c.proxyEnv()...),
					SecurityContext: &corev1.SecurityContext{RunAsUser: &uid, RunAsNonRoot: &rootless},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "sock", MountPath: "/run/buildkit"},
						{Name: "ctx", MountPath: "/ctx", ReadOnly: true},
						{Name: "meta", MountPath: "/meta"},
					},
					Resources: buildResources("1", "1Gi"),
				},
			},
		},
	}
}

// buildResources returns requests+limits (the namespace quota requires both).
func buildResources(cpu, mem string) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(mem),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(mem),
		},
	}
}

// tailBuild streams the log file to logf until the status file appears.
func (c *Client) tailBuild(ctx context.Context, logPath, statusPath string, logf func(string)) error {
	deadline := time.Now().Add(30 * time.Minute)
	offset := int64(0)
	for time.Now().Before(deadline) {
		if f, err := os.Open(logPath); err == nil {
			if _, err := f.Seek(offset, 0); err == nil {
				buf := make([]byte, 32*1024)
				for {
					n, rerr := f.Read(buf)
					if n > 0 {
						if logf != nil {
							for _, line := range splitLines(string(buf[:n])) {
								logf(line)
							}
						}
						offset += int64(n)
					}
					if rerr != nil {
						break
					}
				}
			}
			f.Close()
		}
		if _, err := os.Stat(statusPath); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	return fmt.Errorf("build timed out")
}

func (c *Client) hostDataRoot() (string, error) {
	if v := os.Getenv("EASYLAB_HOST_DATA_DIR"); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("EASYLAB_HOST_DATA_DIR unset (host path of the easylab /data mount)")
}

// hostPath maps an in-container data path (/data/...) to its host path.
func (c *Client) hostPath(containerPath string) string {
	root, _ := c.hostDataRoot()
	dataDir := dataDir()
	rel := containerPath
	if len(rel) >= len(dataDir) && rel[:len(dataDir)] == dataDir {
		rel = rel[len(dataDir):]
	}
	return filepath.Join(root, rel)
}

func hpPtr(t corev1.HostPathType) *corev1.HostPathType { return &t }
func boolPtr(b bool) *bool                             { return &b }
