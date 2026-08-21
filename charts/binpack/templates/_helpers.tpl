{{- define "binpack.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "binpack.fullname" -}}
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

{{- define "binpack.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "binpack.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "binpack.selectorLabels" -}}
app.kubernetes.io/name: {{ include "binpack.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
binpack.serviceAccountName refuses to fall back to the namespace's `default`
ServiceAccount.

Falling back to it is `helm create`'s scaffold, and it reads as safe. It is the
opposite: `default` is the identity every pod in a namespace gets when it names
none, so binding binpack's roles to it hands cluster-wide `nodes: patch` and
`pods/eviction: create` to workloads that never asked for them. binpack itself
runs perfectly well as `default`, so nothing reports the over-grant — the
install works, wrongly, which is the same shape as the combination
binpack.validate refuses.

Refused rather than defaulted to binpack.fullname, because an operator managing
ServiceAccounts elsewhere has not created the account that fallback would name,
and a binding to an account nobody has created grants nothing at all. That is
just the other way to install cleanly and not work.
*/}}
{{- define "binpack.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "binpack.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- required "serviceAccount.create is false but serviceAccount.name is empty: the chart would bind binpack's roles to the namespace's default ServiceAccount, which every pod that names no account of its own already has. Set serviceAccount.name to the account you manage, or leave serviceAccount.create: true." .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}
