package opsext

// ContainerInfo is the ops-extension view of a sandbox worker (sourced from
// easylab SandboxService instead of client-go pod listings).
type ContainerInfo struct {
	ContainerID string
	PodName     string
	Namespace   string
	WorkerURL   string
	PodIP       string
	Status      string
	SessionName string
}
