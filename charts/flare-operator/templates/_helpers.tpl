{{/* Chart name. */}}
{{- define "flare-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Full name prefix for every resource. Truncated to 50 characters so suffixed names
(e.g. -aggregate-to-view, -flarefake) stay within 63.
*/}}
{{- define "flare-operator.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 50 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 50 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 50 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "flare-operator.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Labels common to every resource. */}}
{{- define "flare-operator.labels" -}}
helm.sh/chart: {{ include "flare-operator.chart" . }}
app.kubernetes.io/name: {{ include "flare-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: flare-operator
{{- end }}

{{/* Selector labels for the manager. */}}
{{- define "flare-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "flare-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: manager
{{- end }}

{{/* Selector labels for flarefake. */}}
{{- define "flare-operator.flarefake.selectorLabels" -}}
app.kubernetes.io/name: {{ include "flare-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: flarefake
{{- end }}

{{- define "flare-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "flare-operator.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "flare-operator.image" -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) }}
{{- end }}

{{- define "flare-operator.flarefake.image" -}}
{{- printf "%s:%s" .Values.flarefake.image.repository (default .Chart.AppVersion .Values.flarefake.image.tag) }}
{{- end }}

{{- define "flare-operator.flarefake.fullname" -}}
{{- printf "%s-flarefake" (include "flare-operator.fullname" .) }}
{{- end }}

{{/*
CloudflareAccount spec.baseURL values that reach the flarefake Service, one per line:
the short (same-namespace) name, <svc>.<ns>.svc, and the fully qualified name. The manager
resolves them from the release namespace, where the Service lives.
*/}}
{{- define "flare-operator.flarefake.baseURLs" -}}
{{- $svc := include "flare-operator.flarefake.fullname" . }}
{{- $port := int .Values.flarefake.port }}
{{- printf "http://%s.%s.svc:%d/client/v4" $svc .Release.Namespace $port }}
{{ printf "http://%s.%s.svc.%s:%d/client/v4" $svc .Release.Namespace .Values.clusterDomain $port }}
{{ printf "http://%s:%d/client/v4" $svc $port }}
{{- end }}
