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

{{/*
binpack.autoscalerNamespace is where the cluster-autoscaler publishes its
status, and therefore where binpack's Role for reading it must be bound.

One value, read in two places: the ConfigMap this renders into tells binpack
where to look, and the Role below grants it the right to. They cannot be
allowed to disagree — a Role in the wrong namespace 403s on binpack's first
read, and nothing in the resulting failure says which of the two is wrong.

Defaulted here as well as in the binary because a values file that clears the
`config` block entirely still has to produce a Role somewhere.

`toString` before `default`, because `default` treats false as empty and
`false` is a legal DNS-1123 label and therefore a legal namespace name. Without
it, `autoscalerNamespace: false` would silently grant the Role in kube-system
while the ConfigMap told binpack to read `false`. Callers must `quote` the
result for the same family of reasons: `metadata.namespace` is a string, and
`true`, `false` and `123` all come back out of unquoted YAML as something else.
*/}}
{{- define "binpack.autoscalerNamespace" -}}
{{- default "kube-system" (toString (dig "discovery" "autoscalerNamespace" "" .Values.config)) -}}
{{- end -}}
