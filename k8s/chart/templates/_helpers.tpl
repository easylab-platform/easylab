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

{{/* Registry host, defaulted from the install namespace if not set. */}}
{{- define "easylab.registryHost" -}}
{{- if .Values.registry.host -}}
{{- .Values.registry.host -}}
{{- else -}}
{{- printf "easylab.%s.svc.cluster.local:80" (include "easylab.namespace" .) -}}
{{- end -}}
{{- end -}}

{{/* All imagePullSecrets (easylab registry plus any referenced externals). */}}
{{- define "easylab.imagePullSecrets" -}}
{{- $names := list -}}
{{- if .Values.imagePullSecrets.easylab.create -}}
{{- $names = append $names (.Values.imagePullSecrets.easylab.name | default "easylab-regcred") -}}
{{- end -}}
{{- range .Values.imagePullSecrets.existing -}}
{{- $names = append $names . -}}
{{- end -}}
{{- toYaml $names -}}
{{- end -}}
