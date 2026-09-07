package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/abcp-sdk/abc-protocol-go/extension"
	"net/url"
)

func (s *server) registerDeployTools(m map[string]extension.ToolSpec) {

	m["service-deploy"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
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
					return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "service '%s' belongs to a different session (%s); use another name", "服务 '%s' 属于其它会话（%s）；请换一个名字", name, owner)
				}
			}
			image = s.qualifyImage(image, defaultTag)
			rr := resourceRequestFromArgs(args)
			body := map[string]interface{}{
				"name":  name,
				"image": image,
				"kind":  "deployment",
				"ports": []map[string]interface{}{{"container": 8080, "service": 80}},
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
			body["namespace"] = s.runtimeNamespace
			resp, err := s.httpPostJSON(ctx, s.base+"/api/v1/ops/services", body)
			if err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "service-deploy failed: %v", "service-deploy 失败：%v", err)
			}
			// Surface the service's in-cluster DNS address + readiness so the
			// sandbox (same runtime namespace) can reach it by name, and the
			// agent knows the endpoint to reference.
			svcHost := fmt.Sprintf("%s.%s.svc.cluster.local", name, s.runtimeNamespace)
			ready := "unknown"
			var pm map[string]interface{}
			if jerr := json.Unmarshal([]byte(resp), &pm); jerr == nil {
				if r, ok := pm["ready"].(bool); ok {
					ready = fmt.Sprintf("%v", r)
				}
			}
			return extension.ToolResultData{Content: lc(ctx, s.ext, sessionName,
				fmt.Sprintf("Deployed '%s' from %s. In-cluster address: http://%s:80 (ready=%s). The sandbox can reach it via this hostname.", name, image, svcHost, ready),
				fmt.Sprintf("已从 %s 部署 '%s'。集群内地址：http://%s:80（ready=%s）。沙箱可直接用该主机名访问。", image, name, svcHost, ready)),
				Data: map[string]interface{}{"name": name, "image": image, "svc": svcHost, "url": "http://" + svcHost + ":80", "ready": ready}}, nil
		},
	}
	m["service-list"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
			all := boolArg(args, "all")
			org := strArg(args, "org")
			repo := strArg(args, "repo")
			kind := strArg(args, "kind")
			u := s.base + "/api/v1/ops/services"
			if ns := strArg(args, "namespace"); ns != "" {
				u += "?namespace=" + url.QueryEscape(ns)
			}
			v, err := s.httpGetJSON(ctx, u)
			if err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "service-list failed: %v", "service-list 失败：%v", err)
			}
			if !all && sessionName != "" {
				if _, _, _, ok := tryParseSession(sessionName); ok && v != "" {
					if filtered := filterServicesBySession(v, sessionName); filtered != "" {
						v = filtered
					}
				}
			}
			// Additional tool-layer filters (org/repo/kind) over the returned
			// annotations/kind. easylab list returns these; we slice here so the
			// caller can narrow to e.g. only formal services (kind=deployment)
			// or a specific org/repo without backend support.
			if org != "" || repo != "" || kind != "" {
				v = filterServicesByExtra(v, org, repo, kind)
			}
			return extension.ToolResultData{Content: v}, nil
		},
	}
}
