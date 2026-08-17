{{/*
binpack.validate refuses the combination that installs cleanly and then does
not work.

Caught at render time rather than at runtime because `helm install` is where
somebody is still watching. A binpack that starts and then fails every eviction
with a 403 looks healthy from the outside: it holds its lease, serves its
metrics, and logs an error nobody is reading.

One rule, whether or not the chart manages RBAC. With rbac.create, the flag
grants the verbs; without it, the flag is the operator saying they have granted
them elsewhere. Refusing rbac.create: false outright would make externally
managed RBAC — which the chart documents and supports — unusable for the only
mode where the permissions matter.
*/}}
{{- define "binpack.validate" -}}
{{- if and (not .Values.config.dryRun) (not .Values.rbac.allowDraining) -}}
{{- if .Values.rbac.create -}}
{{- fail "config.dryRun is false but rbac.allowDraining is false: binpack would decide to drain a node and then be refused by RBAC on every attempt. Set rbac.allowDraining: true, or leave config.dryRun: true." -}}
{{- else -}}
{{- fail "config.dryRun is false, rbac.create is false, and rbac.allowDraining is false. binpack needs patch on nodes and create on pods/eviction; grant them wherever you manage RBAC, then set rbac.allowDraining: true to confirm they exist." -}}
{{- end -}}
{{- end -}}
{{- end -}}
