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
