package main

// ResourcePair is one side of a nested resource declaration.
type ResourcePair struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
}

// ResourceRequest is the nested `resources:{requests:{...},limits:{...}}`
// shape accepted by the deploy API and the service-deploy tool.
type ResourceRequest struct {
	Requests *ResourcePair `json:"requests,omitempty"`
	Limits   *ResourcePair `json:"limits,omitempty"`
}
