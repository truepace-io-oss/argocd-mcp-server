{{- define "argocd-mcp.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "argocd-mcp.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s" (include "argocd-mcp.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "argocd-mcp.labels" -}}
app.kubernetes.io/name: {{ include "argocd-mcp.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "argocd-mcp.selectorLabels" -}}
app.kubernetes.io/name: {{ include "argocd-mcp.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "argocd-mcp.serviceAccountName" -}}
{{ include "argocd-mcp.fullname" . }}
{{- end -}}

{{/* Secret name that holds one remote instance's token + ca.crt. */}}
{{- define "argocd-mcp.remoteSecretName" -}}
{{- printf "%s-instance-%s" (include "argocd-mcp.fullname" .root) .instance.name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Every namespace the local instance needs RBAC in: the Argo CD namespace plus any
additional application namespaces ("apps in any namespace").
*/}}
{{- define "argocd-mcp.localNamespaces" -}}
{{- $ns := list .Values.localInstance.namespace -}}
{{- range .Values.localInstance.applicationNamespaces -}}
{{- $ns = append $ns . -}}
{{- end -}}
{{- $ns | uniq | toJson -}}
{{- end -}}
