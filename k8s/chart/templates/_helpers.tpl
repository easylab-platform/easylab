{{/* Namespace: release namespace unless overridden. */}}
{{- define "easylab.namespace" -}}
{{- .Values.namespaceOverride | default .Release.Namespace -}}
{{- end -}}

{{/* Base name. */}}
{{- define "easylab.name" -}}
{{- .Values.nameOverride | default "easylab" -}}
{{- end -}}

{{/* Runtime namespace (same as install namespace). */}}
{{- define "easylab.runtimeNamespace" -}}
{{- include "easylab.namespace" . -}}
{{- end -}}

{{/* Common labels. */}}
{{- define "easylab.labels" -}}
app.kubernetes.io/name: {{ include "easylab.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end -}}

{{/* Component selector labels. */}}
{{- define "easylab.selectorLabels" -}}
app.kubernetes.io/name: {{ include "easylab.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* ServiceAccount name. */}}
{{- define "easylab.serviceAccountName" -}}
{{- if .Values.rbac.serviceAccount.create -}}
{{- .Values.rbac.serviceAccount.name | default "easylab" -}}
{{- else -}}
{{- .Values.rbac.serviceAccount.name | default "default" -}}
{{- end -}}
{{- end -}}

{{/* Full image refs. */}}
{{- define "easylab.gatewayImage" -}}
{{- printf "%s:%s" .Values.gateway.image.repository (.Values.gateway.image.tag | toString) -}}
{{- end -}}
{{- define "easylab.natsImage" -}}
{{- printf "%s:%s" .Values.nats.image.repository (.Values.nats.image.tag | toString) -}}
{{- end -}}
{{- define "easylab.agentImage" -}}
{{- printf "%s:%s" .Values.agent.image.repository (.Values.agent.image.tag | toString) -}}
{{- end -}}

{{/* S3 object-store env for the AGENT only. Durable file bytes move out of
   NATS into an S3-compatible bucket; file metadata moves to the agent's
   `agent_files` DB table. Extensions never read the store directly — they go
   through the agent's GetFile RPC — so they need no object-store credentials. */}}
{{- define "easylab.objectStoreEnv" -}}
{{- if .Values.objectStore.enabled }}
- name: AGENT_BLOB_BACKEND
  value: "s3"
- name: S3_BUCKET
  value: {{ .Values.objectStore.bucket | quote }}
- name: S3_REGION
  value: {{ .Values.objectStore.region | quote }}
- name: S3_ENDPOINT
  value: {{ .Values.objectStore.endpoint | quote }}
- name: S3_ACCESS_KEY
  value: {{ .Values.objectStore.accessKey | quote }}
- name: S3_SECRET_KEY
  value: {{ .Values.objectStore.secretKey | quote }}
- name: S3_PATH_STYLE
  value: {{ .Values.objectStore.pathStyle | toString | quote }}
- name: S3_PREFIX
  value: {{ .Values.objectStore.prefix | quote }}
{{- end }}
{{- end -}}

{{/* Registry host (TLS ingress name nodes already trust); falls back to the
   in-cluster Service DNS when unset. */}}
{{- define "easylab.registryHost" -}}
{{- if .Values.registry.host -}}
{{- .Values.registry.host -}}
{{- else -}}
{{- printf "easylab.%s.svc.cluster.local:80" (include "easylab.namespace" .) -}}
{{- end -}}
{{- end -}}

{{/* External base URL advertised to clients; defaults to the in-cluster
   Service URL (http://easylab.<ns>.svc.cluster.local). */}}
{{- define "easylab.selfBase" -}}
{{- if .Values.gateway.selfBase -}}
{{- .Values.gateway.selfBase -}}
{{- else -}}
{{- printf "http://easylab.%s.svc.cluster.local" (include "easylab.namespace" .) -}}
{{- end -}}
{{- end -}}

{{/* Artifact (OCI + /pkgs) base URL used by builds; defaults to the gateway
   Service over plain HTTP. */}}
{{- define "easylab.artifactURL" -}}
{{- if .Values.gateway.artifactURL -}}
{{- .Values.gateway.artifactURL -}}
{{- else -}}
{{- printf "http://easylab.%s.svc.cluster.local:80" (include "easylab.namespace" .) -}}
{{- end -}}
{{- end -}}

{{/* Sandbox runtime overrides as JSON for EASYLAB_SANDBOX_RUNTIMES. Each
   runtime's image defaults to <vmImagePrefix>/easyworker-<runtime>:<vmTag>
   unless the runtime already sets one (so the VM tag lives in one place). */}}
{{- define "easylab.sandboxRuntimes" -}}
{{- $prefix := .Values.sandboxes.vmImagePrefix -}}
{{- $tag := .Values.sandboxes.vmTag | toString -}}
{{- $out := dict -}}
{{- range $name, $cfg := .Values.sandboxes.runtimes -}}
{{- $c := deepCopy $cfg -}}
{{- if not $c.image -}}
{{- $_ := set $c "image" (printf "%s/easyworker-%s:%s" $prefix $name $tag) -}}
{{- end -}}
{{- $_ := set $out $name $c -}}
{{- end -}}
{{- $out | toJson -}}
{{- end -}}
