{{/*
Common template helpers. Names are stable across upgrades — changing any of
these on an existing release will recreate the corresponding resources,
which is usually NOT what you want for a CSI driver (DaemonSets and the
CSIDriver object are referenced by kubelet's plugin registration).
*/}}

{{/* Fully-qualified name (release + chart). */}}
{{- define "local-path-csi.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := .Chart.Name -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "local-path-csi.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "local-path-csi.namespace" -}}
{{- default .Release.Namespace .Values.namespaceOverride -}}
{{- end -}}

{{- define "local-path-csi.controllerName" -}}
{{ include "local-path-csi.fullname" . }}-controller
{{- end -}}

{{- define "local-path-csi.nodeName" -}}
{{ include "local-path-csi.fullname" . }}-node
{{- end -}}

{{- define "local-path-csi.nodeWindowsName" -}}
{{ include "local-path-csi.fullname" . }}-node-windows
{{- end -}}

{{- define "local-path-csi.helperSAName" -}}
{{ include "local-path-csi.fullname" . }}-helper
{{- end -}}

{{- define "local-path-csi.driverImage" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}

{{- define "local-path-csi.helperImage" -}}
{{ .Values.helperImage.repository }}:{{ .Values.helperImage.tag | default .Chart.AppVersion }}
{{- end -}}

{{- define "local-path-csi.labels" -}}
app.kubernetes.io/name: {{ include "local-path-csi.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
app.kubernetes.io/component: csi
{{- end -}}

