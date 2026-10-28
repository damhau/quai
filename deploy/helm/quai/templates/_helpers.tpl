{{/* The release's name for its objects, at most 63 characters. */}}
{{- define "quai.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "quai.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "quai.selector" -}}
app.kubernetes.io/name: {{ .root.Chart.Name }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{- define "quai.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "quai.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "quai.image" -}}
{{ .Values.image.repository }}:{{ default .Chart.AppVersion .Values.image.tag }}
{{- end -}}

{{/* The environment of every process: the database, the CA bundle, the proxy. */}}
{{- define "quai.env" -}}
- name: QUAI_DATABASE_URL
  valueFrom: { secretKeyRef: { name: {{ .Values.database.secretName }}, key: {{ .Values.database.secretKey }} } }
{{- with .Values.network.caBundle.configMap }}
- name: QUAI_NETWORK_CA_FILE
  value: /etc/quai-ca/{{ $.Values.network.caBundle.key }}
{{- end }}
{{- with .Values.network.proxy }}
{{- if .https }}
- { name: HTTPS_PROXY, value: {{ .https | quote }} }
{{- end }}
{{- if .http }}
- { name: HTTP_PROXY, value: {{ .http | quote }} }
{{- end }}
{{- if .noProxy }}
- { name: NO_PROXY, value: {{ .noProxy | quote }} }
{{- end }}
{{- end }}
{{- end -}}

{{/* The container of the server and the worker: image, configuration, secrets. */}}
{{- define "quai.container" -}}
image: {{ include "quai.image" .root }}
imagePullPolicy: {{ .root.Values.image.pullPolicy }}
args: [{{ .command | quote }}]
env:
  - name: QUAI_CONFIG
    value: /etc/quai/quai.yaml
  {{- include "quai.env" .root | nindent 2 }}
  {{- if eq (dig "auth" "mode" "" .root.Values.config) "oidc" }}
  - name: QUAI_AUTH_OIDC_CLIENT_SECRET
    valueFrom: { secretKeyRef: { name: {{ .root.Values.oidc.secretName }}, key: {{ .root.Values.oidc.secretKey }} } }
  {{- end }}
securityContext: {{- toYaml .root.Values.securityContext | nindent 2 }}
volumeMounts:
  - { name: config, mountPath: /etc/quai, readOnly: true }
  - { name: tmp, mountPath: /tmp }
  - { name: cache, mountPath: /var/cache/quai }
  {{- if .root.Values.network.caBundle.configMap }}
  - { name: ca, mountPath: /etc/quai-ca, readOnly: true }
  {{- end }}
{{- end -}}

{{- define "quai.volumes" -}}
- name: config
  {{- if .root.Values.configSecret }}
  secret: { secretName: {{ .root.Values.configSecret }} }
  {{- else }}
  configMap: { name: {{ include "quai.fullname" .root }} }
  {{- end }}
- name: tmp
  emptyDir: {}
{{- with .root.Values.network.caBundle.configMap }}
- name: ca
  configMap: { name: {{ . }} }
{{- end }}
- name: cache
  emptyDir: {{ if .sizeLimit }}{ sizeLimit: {{ .sizeLimit }} }{{ else }}{}{{ end }}
{{- end -}}

{{/* The pod settings. A hook runs before the release's service account exists, and needs none. */}}
{{- define "quai.pod" -}}
{{- if .hook -}}
automountServiceAccountToken: false
{{- else -}}
serviceAccountName: {{ include "quai.serviceAccountName" .root }}
automountServiceAccountToken: {{ .root.Values.secretStore.kubernetes.enabled }}
{{- end }}
securityContext: {{- toYaml .root.Values.podSecurityContext | nindent 2 }}
{{- with .root.Values.imagePullSecrets }}
imagePullSecrets: {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .root.Values.nodeSelector }}
nodeSelector: {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .root.Values.tolerations }}
tolerations: {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .root.Values.affinity }}
affinity: {{- toYaml . | nindent 2 }}
{{- end }}
{{- end -}}
