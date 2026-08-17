{{/*
binpack.validate refuses the two combinations that install cleanly and then do
not work.

Caught at render time rather than at runtime because `helm install` is where
somebody is still watching. A binpack that starts and then fails every eviction
with a 403 looks healthy from the outside: it holds its lease, serves its
metrics, and logs an error nobody is reading.
*/}}
{{- define "binpack.validate" -}}
{{- if and (not .Values.config.dryRun) .Values.rbac.create (not .Values.rbac.allowDraining) -}}
{{- fail "config.dryRun is false but rbac.allowDraining is false: binpack would decide to drain a node and then be refused by RBAC on every attempt. Set rbac.allowDraining: true, or leave config.dryRun: true." -}}
{{- end -}}
{{- if and (not .Values.config.dryRun) (not .Values.rbac.create) -}}
{{- fail "config.dryRun is false and rbac.create is false: binpack needs patch on nodes and create on pods/eviction. Grant them wherever you manage RBAC, then set both if you are sure they exist." -}}
{{- end -}}
{{- end -}}
