{{/*
Expand the name of the chart.
*/}}
{{- define "dra-driver-nvidia-gpu.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "dra-driver-nvidia-gpu.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Allow the release namespace to be overridden for multi-namespace deployments in combined charts
*/}}
{{- define "dra-driver-nvidia-gpu.namespace" -}}
  {{- if .Values.namespaceOverride -}}
    {{- .Values.namespaceOverride -}}
  {{- else -}}
    {{- .Release.Namespace -}}
  {{- end -}}
{{- end -}}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "dra-driver-nvidia-gpu.chart" -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- printf "%s-%s" $name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Standard labels: documented at
https://helm.sh/docs/chart_best_practices/labels/
Apply this to all high-level objects (Deployment, DaemonSet, ...).
Pod template labels are included here to deliver name+instance.
*/}}
{{- define "dra-driver-nvidia-gpu.labels" -}}
helm.sh/chart: {{ include "dra-driver-nvidia-gpu.chart" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{ include "dra-driver-nvidia-gpu.templateLabels" . }}
{{- end }}

{{/*
Apply this to all pod templates (a smaller set of labels compared to
the set of standard labels above, to not clutter individual pods too
much). Note that these labels cannot be used to distinguish
components within this Helm chart.
*/}}
{{- define "dra-driver-nvidia-gpu.templateLabels" -}}
app.kubernetes.io/name: {{ include "dra-driver-nvidia-gpu.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Selector label: precisely filter for just the pods of the corresponding
Deployment, DaemonSet, .... That is, this label key/value pair must be
different per-component (a component name is a required argument). This
could be many labels, but we want to use just one (with a sufficiently
unique key).

TOOD: remove the override feature, or make the override work per-component.
*/}}
{{- define "dra-driver-nvidia-gpu.selectorLabels" -}}
{{- if and (hasKey . "componentName") (hasKey . "context") -}}
{{- if .context.Values.selectorLabelsOverride -}}
{{ toYaml .context.Values.selectorLabelsOverride }}
{{- else -}}
{{- $name := default .context.Chart.Name .context.Values.nameOverride -}}
{{ $name }}-component: {{ .componentName }}
{{- end }}
{{- else -}}
fail "selectorLabels: both arguments are required: context, componentName"
{{- end }}
{{- end }}

{{/*
Full image name with tag
*/}}
{{- define "dra-driver-nvidia-gpu.fullimage" -}}
{{- $tag := printf "v%s" .Chart.AppVersion }}
{{- .Values.image.repository -}}:{{- .Values.image.tag | default $tag -}}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "dra-driver-nvidia-gpu.serviceAccountName" -}}
{{- $name := printf "%s-service-account" (include "dra-driver-nvidia-gpu.fullname" .) }}
{{- if .Values.serviceAccount.create }}
{{- default $name .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Create the name of the webhook service account to use
*/}}
{{- define "dra-driver-nvidia-gpu.webhookServiceAccountName" -}}
{{- $name := printf "%s-webhook-service-account" (include "dra-driver-nvidia-gpu.fullname" .) }}
{{- if .Values.webhook.serviceAccount.create }}
{{- default $name .Values.webhook.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.webhook.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Check for the existence of an element in a list
*/}}
{{- define "dra-driver-nvidia-gpu.listHas" -}}
  {{- $listToCheck := index . 0 }}
  {{- $valueToCheck := index . 1 }}

  {{- $found := "" -}}
  {{- range $listToCheck}}
    {{- if eq . $valueToCheck }}
      {{- $found = "true" -}}
    {{- end }}
  {{- end }}
  {{- $found -}}
{{- end }}

{{/*
Filter a list by a set of valid values
*/}}
{{- define "dra-driver-nvidia-gpu.filterList" -}}
  {{- $listToFilter := index . 0 }}
  {{- $validValues := index . 1 }}

  {{- $result := list -}}
  {{- range $validValues}}
    {{- if include "dra-driver-nvidia-gpu.listHas" (list $listToFilter .) }}
      {{- $result = append $result . }}
    {{- end }}
  {{- end }}
  {{- $result -}}
{{- end -}}

{{/*
Get all namespaces (driver namespace + additional namespaces from environment variable).
After concatenation, duplicates from are removed with uniq to avoid release namespaces been
listed in ADDITIONAL_NAMESPACES, or repeated entries in the comma-separated list.
*/}}
{{- define "dra-driver-nvidia-gpu.namespaces" -}}
  {{- $driverNs := include "dra-driver-nvidia-gpu.namespace" . | trim }}
    {{- $namespaces := list $driverNs }}
    {{- if .Values.controller.containers.computeDomain.env }}
      {{- range .Values.controller.containers.computeDomain.env }}
        {{- if eq .name "ADDITIONAL_NAMESPACES" }}
          {{- if .value }}
            {{- range $raw := splitList "," .value }}
              {{- $ns := $raw | trim }}
              {{- if $ns }}
                  {{- $namespaces = concat $namespaces (list $ns) }}
              {{- end }}
            {{- end }}
          {{- end }}
      {{- end }}
    {{- end }}
  {{- end }}
  {{- join "," (uniq $namespaces) -}}
{{- end -}}

{{/*
Get the latest available resource.k8s.io API version

Priority:
  1. If .Values.resourceApiVersion is set, use that.
  2. Otherwise, returns the highest available version or empty string if none found
*/}}
{{- define "dra-driver-nvidia-gpu.resourceApiVersion" -}}
{{- if .Values.resourceApiVersion }}
{{- .Values.resourceApiVersion }}
{{- else if .Capabilities.APIVersions.Has "resource.k8s.io/v1" -}}
resource.k8s.io/v1
{{- else if .Capabilities.APIVersions.Has "resource.k8s.io/v1beta2" -}}
resource.k8s.io/v1beta2
{{- else if .Capabilities.APIVersions.Has "resource.k8s.io/v1beta1" -}}
resource.k8s.io/v1beta1
{{- else -}}
{{- end -}}
{{- end -}}

{{/*
HostManagedIMEX feature gate state.

Returns the string "true" only when the HostManagedIMEX feature gate is
explicitly enabled in .Values.featureGates (boolean true or the string
"true"), and an empty string otherwise. Empty string is falsy in Helm
conditionals, so callers can write:
  {{- if not (include "dra-driver-nvidia-gpu.hostManagedIMEX" .) }}
The explicit "true" comparison is required so that
--set-string featureGates.HostManagedIMEX=false (a non-empty, hence
truthy, string) is not mistaken for enabled.
*/}}
{{- define "dra-driver-nvidia-gpu.hostManagedIMEX" -}}
{{- if eq (toString (dig "HostManagedIMEX" false (.Values.featureGates | default dict))) "true" -}}
true
{{- end -}}
{{- end -}}
