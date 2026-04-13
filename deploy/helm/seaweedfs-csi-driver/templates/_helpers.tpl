{{- define "seaweedfs-csi-driver.name" -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Whether a valid security.toml source is configured.
*/}}
{{- define "seaweedfs-csi-driver.security-enabled" -}}
{{- if .Values.security.enabled -}}
{{- if not (or .Values.security.existingSecret .Values.security.existingConfigMap (and .Values.security.create .Values.security.content)) -}}
{{- fail "security.enabled is true but no source configured. Set one of: security.existingSecret, security.existingConfigMap, or security.create with security.content." -}}
{{- end -}}
true
{{- end -}}
{{- end -}}

{{/*
Security volume definition.
Produces a volume entry sourcing security.toml from the appropriate Secret or ConfigMap.
*/}}
{{- define "seaweedfs-csi-driver.security-volume" -}}
{{- if include "seaweedfs-csi-driver.security-enabled" . -}}
- name: security-config
{{- if .Values.security.existingSecret }}
  secret:
    secretName: {{ .Values.security.existingSecret }}
{{- else if .Values.security.existingConfigMap }}
  configMap:
    name: {{ .Values.security.existingConfigMap }}
{{- else if eq .Values.security.type "secret" }}
  secret:
    secretName: {{ template "seaweedfs-csi-driver.name" . }}-security-config
{{- else if eq .Values.security.type "configmap" }}
  configMap:
    name: {{ template "seaweedfs-csi-driver.name" . }}-security-config
{{- else }}
{{- fail (printf "security.type must be \"secret\" or \"configmap\", got: %s" .Values.security.type) -}}
{{- end }}
{{- end }}
{{- end -}}

{{/*
Security volumeMount definition.
Mounts security.toml to /etc/seaweedfs/security.toml.
*/}}
{{- define "seaweedfs-csi-driver.security-volumemount" -}}
{{- if include "seaweedfs-csi-driver.security-enabled" . -}}
- name: security-config
  mountPath: /etc/seaweedfs/security.toml
  subPath: security.toml
  readOnly: true
{{- end }}
{{- end -}}
