package opsext

import (
	"context"
	"encoding/json"
	"fmt"

	"connectrpc.com/connect"

	"github.com/abcp-sdk/abc-protocol-go/v2/extension"
	easylabv1 "github.com/easylab-platform/easylab-proto/easylab/v1"
	"github.com/easylab-platform/easylab/internal/ext"
)

func (s *server) registerDeployTools(m map[string]extension.ToolSpec) {

	m["service-deploy"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string, tenant string) (extension.ToolResultData, error) {
			ctx = ext.WithLabTenant(ctx, tenant)
			image := strArg(args, "image")
			name := strArg(args, "name")
			defaultTag := ""
			org, repo, bm := "", "", ""
			if sessionName != "" {
				if o, r, b, ok := parseSessionName(sessionName); ok {
					org, repo, bm = o, r, b
					defaultTag = b
				}
			}
			// Default the service name to a k8s-safe, globally-unique slug so
			// deployments never collide across orgs/repos/sessions. A caller
			// may still pass an explicit `name` (ownership check still applies).
			if name == "" && org != "" && repo != "" && bm != "" {
				name = k8sServiceName(org, repo, bm)
			}
			if name == "" {
				name = "app"
			}
			// Ownership guard: only the session that created a service may
			// update/scale it. Read the existing service's session annotation
			// (easylab status now returns `annotations`); absent annotation
			// (legacy/service, no easylab/session) is adopted + tagged on first
			// touch; a mismatched session is rejected with a conflict.
			existing, err := s.fetchService(ctx, name, s.runtimeNamespace)
			if err == nil && existing != nil {
				owner, _ := existing["session"].(string)
				if owner != "" && owner != sessionName {
					return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, MsgServiceArgBelongsToADifferentSessionArg, name, owner)
				}
			}
			image = s.qualifyImage(image, defaultTag)
			rr := resourceRequestFromArgs(args)
			body := map[string]interface{}{
				"name":  name,
				"image": image,
				"kind":  "deployment",
				// Gateway ServiceRequest.Ports is map[int]int (container port
				// -> published host port) — an array here decodes to 400.
				"ports": map[int]int{8080: 80},
			}
			ann := map[string]string{}
			if sessionName != "" {
				ann["easylab/session"] = sessionName
			}
			if org != "" {
				ann["easylab/org"] = org
			}
			if repo != "" {
				ann["easylab/repo"] = repo
			}
			if len(ann) > 0 {
				body["annotations"] = ann
			}
			if env := envMapFromArgs(args); len(env) > 0 {
				body["env"] = env
			}
			if rr.Requests != nil || rr.Limits != nil {
				res := map[string]interface{}{}
				if rr.Requests != nil {
					res["cpu"] = rr.Requests.CPU
					res["memory"] = rr.Requests.Memory
				}
				body["resources"] = res
			}
			_ = json.Marshal
			_, lerr := s.sdk.Ops.LaunchService(ctx, connect.NewRequest(&easylabv1.LaunchServiceRequest{
				Name: name, Image: image, Kind: "deployment",
				Namespace:   s.runtimeNamespace,
				Ports:       []*easylabv1.PortSpec{{Container: 8080, Service: 80}},
				Annotations: ann,
			}))
			if lerr != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, MsgServiceDeployFailedArg, lerr)
			}
			// Surface the service's in-cluster DNS address so the sandbox (same
			// runtime namespace) can reach it by name.
			svcHost := fmt.Sprintf("%s.%s.svc.cluster.local", name, s.runtimeNamespace)
			ready := "unknown"
			return extension.ToolResultData{Content: lcf(ctx, s.ext, tenant, sessionName, MsgDeployedArgFromArgInClusterAddressHttp, name, image, svcHost, ready),
				Data: map[string]interface{}{"name": name, "image": image, "svc": svcHost, "url": "http://" + svcHost + ":80", "ready": ready}}, nil
		},
	}
	m["service-list"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string, tenant string) (extension.ToolResultData, error) {
			ctx = ext.WithLabTenant(ctx, tenant)
			all := boolArg(args, "all")
			org := strArg(args, "org")
			repo := strArg(args, "repo")
			kind := strArg(args, "kind")
			res, err := s.sdk.Ops.ListServices(ctx, connect.NewRequest(&easylabv1.ListServicesRequest{
				Namespace: strArg(args, "namespace"),
			}))
			if err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, MsgServiceListFailedArg, err)
			}
			entries := make([]map[string]interface{}, 0, len(res.Msg.GetServices()))
			for _, svc := range res.Msg.GetServices() {
				entries = append(entries, map[string]interface{}{
					"name": svc.GetName(), "kind": svc.GetKind(), "phase": svc.GetPhase(),
					"replicas": svc.GetReplicas(), "ready": svc.GetReady(),
					"url": svc.GetUrl(), "service_url": svc.GetUrl(), "worker_url": svc.GetImage(),
					"session": svc.GetSession(), "org": svc.GetOrg(), "repo": svc.GetRepo(),
				})
			}
			raw, _ := json.Marshal(map[string]interface{}{"services": entries})
			v := string(raw)
			if !all && sessionName != "" {
				if _, _, _, ok := tryParseSession(sessionName); ok && v != "" {
					if filtered := filterServicesBySession(v, sessionName); filtered != "" {
						v = filtered
					}
				}
			}
			if org != "" || repo != "" || kind != "" {
				v = filterServicesByExtra(v, org, repo, kind)
			}
			return extension.ToolResultData{Content: v}, nil
		},
	}
}
