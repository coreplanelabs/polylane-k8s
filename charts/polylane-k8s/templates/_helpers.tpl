{{- define "polylane-k8s.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "polylane-k8s.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "polylane-k8s.labels" -}}
app.kubernetes.io/name: {{ include "polylane-k8s.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.Version | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "polylane-k8s.selectorLabels" -}}
app.kubernetes.io/name: {{ include "polylane-k8s.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "polylane-k8s.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "polylane-k8s.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
stateSecretName is referenced by the Secret itself, the Role's
resourceNames pin, the cloudflared volume, and the rendered config —
one helper so they can never drift apart.
*/}}
{{- define "polylane-k8s.stateSecretName" -}}
{{- default (printf "%s-state" (include "polylane-k8s.fullname" .)) .Values.stateSecret.name -}}
{{- end -}}

{{- define "polylane-k8s.apiKeySecretName" -}}
{{- if .Values.apiKey.existingSecret -}}
{{- .Values.apiKey.existingSecret -}}
{{- else -}}
{{- printf "%s-api-key" (include "polylane-k8s.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
image/tunnelImage: a non-empty digest wins over the tag (Renovate pins
digests; tag-only stays readable until then).
*/}}
{{- define "polylane-k8s.image" -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.Version .Values.image.tag) -}}
{{- end -}}
{{- end -}}

{{- define "polylane-k8s.tunnelImage" -}}
{{- if .Values.tunnel.image.digest -}}
{{- printf "%s@%s" .Values.tunnel.image.repository .Values.tunnel.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.tunnel.image.repository .Values.tunnel.image.tag -}}
{{- end -}}
{{- end -}}

{{/*
config renders the agent's config.yaml: .Values.config verbatim over the
chart-derived defaults, with state_secret.name always forced to the
Secret this chart creates and RBAC-pins (rightmost mergeOverwrite wins).
*/}}
{{- define "polylane-k8s.config" -}}
{{- $defaults := dict "shim" (dict "listen" "127.0.0.1:8080") "ops" (dict "health_listen" ":8081" "metrics_listen" ":9090") -}}
{{- $derived := dict "state_secret" (dict "name" (include "polylane-k8s.stateSecretName" .)) -}}
{{- toYaml (mustMergeOverwrite $defaults (deepCopy (default (dict) .Values.config)) $derived) -}}
{{- end -}}
