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

{{/*
workersLogs with the chart defaults filled in, as YAML (use: include ... | fromYaml).
`helm upgrade --reuse-values --set workersLogs.enabled=true` from a chart version without
workersLogs passes only "enabled"; the defaults here must equal values.yaml
(test/chart TestWorkersLogsReuseValues).
*/}}
{{- define "flare-operator.workersVK.values" -}}
{{- $w := .Values.workersLogs | default dict }}
{{- /* dig per field, not merge: sprig's merge treats false as unset (approve: false would become true). */}}
{{- $v := dict
  "enabled" (dig "enabled" false $w)
  "nodeName" (dig "nodeName" "cf-workers" $w)
  "namespaceSelector" (dig "namespaceSelector" dict $w)
  "hostNetwork" (dig "hostNetwork" false $w)
  "port" (dig "port" 10250 $w)
  "tls" (dict
    "mode" (dig "tls" "mode" "csr" $w)
    "secretName" (dig "tls" "secretName" "" $w)
    "csr" (dict "approve" (dig "tls" "csr" "approve" true $w) "lifetime" (dig "tls" "csr" "lifetime" "24h" $w)))
  "podImage" (dig "podImage" "registry.k8s.io/pause:3.10" $w)
  "podResources" (dict "cpu" (dig "podResources" "cpu" "1m" $w) "memory" (dig "podResources" "memory" "1Mi" $w))
  "podLabels" (dig "podLabels" dict $w)
  "apiBudget" (dig "apiBudget" 120 $w)
  "logs" (dict
    "defaultWindow" (dig "logs" "defaultWindow" "72h" $w)
    "maxEvents" (dig "logs" "maxEvents" 10000 $w)
    "maxFollowers" (dig "logs" "maxFollowers" 100 $w))
  "networkPolicy" (dict "kubeletAPIFrom" (dig "networkPolicy" "kubeletAPIFrom" list $w))
  "extraArgs" (dig "extraArgs" list $w)
  "resources" (dig "resources" (dict "requests" (dict "cpu" "10m" "memory" "64Mi") "limits" (dict "memory" "256Mi")) $w)
  "nodeSelector" (dig "nodeSelector" dict $w)
  "tolerations" (dig "tolerations" list $w)
  "affinity" (dig "affinity" dict $w) }}
{{- toYaml $v }}
{{- end }}

{{/* Workers virtual kubelet (workersLogs): resource name, also the Node's owner ClusterRole. */}}
{{- define "flare-operator.workersVK.fullname" -}}
{{- printf "%s-workers-vk" (include "flare-operator.fullname" .) }}
{{- end }}

{{/* Selector labels for the Workers virtual kubelet. */}}
{{- define "flare-operator.workersVK.selectorLabels" -}}
app.kubernetes.io/name: {{ include "flare-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: workers-vk
{{- end }}

{{/*
Kubelet API port (context: the workersVK.values dict): port, except that with hostNetwork the default 10250 (the real
kubelet's) becomes 10260.
*/}}
{{- define "flare-operator.workersVK.port" -}}
{{- $port := int .port }}
{{- if and .hostNetwork (eq $port 10250) }}{{ $port = 10260 }}{{ end }}
{{- $port }}
{{- end }}

{{/* Health probe port (context: the workersVK.values dict): 8081 in the Pod's own network namespace, 10261 on the host network. */}}
{{- define "flare-operator.workersVK.probePort" -}}
{{- if .hostNetwork }}10261{{ else }}8081{{ end }}
{{- end }}

{{/*
namespaceSelector of the workersVK.values dict (matchLabels/matchExpressions) as a label selector string
("" selects every namespace), for --namespace-selector.
*/}}
{{- define "flare-operator.workersVK.namespaceSelector" -}}
{{- $sel := .namespaceSelector | default dict }}
{{- $parts := list }}
{{- range $k, $v := ($sel.matchLabels | default dict) }}
{{- $parts = append $parts (printf "%s=%s" $k $v) }}
{{- end }}
{{- range ($sel.matchExpressions | default list) }}
{{- $vals := join "," (.values | default list) }}
{{- if and (has .operator (list "In" "NotIn")) (not .values) }}{{ fail (printf "workersLogs.namespaceSelector.matchExpressions: operator %s for key %q needs values" .operator (toString .key)) }}{{ end }}
{{- if eq .operator "In" }}{{ $parts = append $parts (printf "%s in (%s)" .key $vals) }}
{{- else if eq .operator "NotIn" }}{{ $parts = append $parts (printf "%s notin (%s)" .key $vals) }}
{{- else if eq .operator "Exists" }}{{ $parts = append $parts .key }}
{{- else if eq .operator "DoesNotExist" }}{{ $parts = append $parts (printf "!%s" .key) }}
{{- else }}{{ fail (printf "workersLogs.namespaceSelector.matchExpressions: unknown operator %q for key %q (In, NotIn, Exists, DoesNotExist)" (toString .operator) (toString .key)) }}
{{- end }}
{{- end }}
{{- join "," $parts }}
{{- end }}
