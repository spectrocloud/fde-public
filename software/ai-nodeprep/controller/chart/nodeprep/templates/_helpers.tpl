{{/* Chart-level naming: fullname respects nameOverride/fullnameOverride. */}}
{{- define "nodeprep.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "nodeprep.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s" (include "nodeprep.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "nodeprep.namespace" -}}
{{- default "nodeprep-system" .Values.namespace.name -}}
{{- end -}}

{{/*
Common labels on every rendered object. The component label distinguishes
controller/agent so workload selectors stay chart-external.
*/}}
{{- define "nodeprep.labels" -}}
app.kubernetes.io/part-of: nodeprep
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "nodeprep.controllerLabels" -}}
{{- include "nodeprep.labels" . }}
app.kubernetes.io/name: {{ include "nodeprep.fullname" . }}-controller
{{- end -}}

{{- define "nodeprep.agentLabels" -}}
{{- include "nodeprep.labels" . }}
app.kubernetes.io/name: {{ include "nodeprep.fullname" . }}-agent
{{- end -}}

{{/*
One image per version, two tags: the Makefile builds <appVersion>-agent and
<appVersion>-controller and never re-pushes a tag. A values tag overrides for
side-loads (e.g. a registry mirror).
*/}}
{{- define "nodeprep.controllerImage" -}}
{{- printf "%s:%s" .Values.controller.image.repository
    (default (printf "%s-controller" .Chart.AppVersion) .Values.controller.image.tag) -}}
{{- end -}}

{{- define "nodeprep.agentImage" -}}
{{- printf "%s:%s" .Values.agent.image.repository
    (default (printf "%s-agent" .Chart.AppVersion) .Values.agent.image.tag) -}}
{{- end -}}

{{/* Merge map lists for nodeSelector/affinity passthrough. */}}
{{- define "nodeprep.selector" -}}
{{- toYaml . | nindent 8 -}}
{{- end -}}
