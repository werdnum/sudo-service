{{/*
Fully-qualified controller image reference. Prefers an explicit digest
(set by CI) over a tag.
*/}}
{{- define "sudo-service.image" -}}
{{- if .Values.image.digest -}}
{{ .Values.image.repository }}@{{ .Values.image.digest }}
{{- else -}}
{{ .Values.image.repository }}:{{ .Values.image.tag }}
{{- end -}}
{{- end -}}

{{/*
Controller ServiceAccount username, as seen by the apiserver. Referenced by the
ValidatingAdmissionPolicies, which are cluster-scoped and cannot use a
namespaceSelector to imply the namespace.
*/}}
{{- define "sudo-service.controllerUsername" -}}
system:serviceaccount:{{ .Values.namespace }}:sudo-service-controller-sa
{{- end -}}
