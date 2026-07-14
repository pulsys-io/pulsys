{{/*
Expand the name of the chart.
*/}}
{{- define "pulsys.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "pulsys.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Chart name and version as used by the chart label.
*/}}
{{- define "pulsys.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "pulsys.labels" -}}
helm.sh/chart: {{ include "pulsys.chart" . }}
{{ include "pulsys.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: pulsys
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "pulsys.selectorLabels" -}}
app.kubernetes.io/name: {{ include "pulsys.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
ServiceAccount name.
*/}}
{{- define "pulsys.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "pulsys.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
The container image reference (tag defaults to appVersion).
*/}}
{{- define "pulsys.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end }}

{{/*
Name of the Secret holding the Postgres DSN. Uses an existing Secret when
provided, otherwise the chart-managed Secret.
*/}}
{{- define "pulsys.dbSecretName" -}}
{{- if .Values.postgres.existingSecret -}}
{{- .Values.postgres.existingSecret -}}
{{- else -}}
{{- printf "%s-db" (include "pulsys.fullname" .) -}}
{{- end -}}
{{- end }}

{{- define "pulsys.dbSecretKey" -}}
{{- if .Values.postgres.existingSecret -}}
{{- .Values.postgres.existingSecretKey | default "dsn" -}}
{{- else -}}
dsn
{{- end -}}
{{- end }}

{{/*
Name of the Secret holding the OIDC client secret.
*/}}
{{- define "pulsys.oidcSecretName" -}}
{{- if .Values.oidc.existingSecret -}}
{{- .Values.oidc.existingSecret -}}
{{- else -}}
{{- printf "%s-oidc" (include "pulsys.fullname" .) -}}
{{- end -}}
{{- end }}

{{/*
Validate required values and fail fast with actionable messages.
*/}}
{{- define "pulsys.validate" -}}
{{- if not .Values.proxy.publicBaseURL -}}
{{- fail "proxy.publicBaseURL is required: set it to the externally reachable URL of the proxy (e.g. https://hf.example.com)" -}}
{{- end -}}
{{- if not .Values.admin.enabled -}}
{{- fail "Pulsys has no open mode: admin.enabled must be true. The admin plane (Postgres) is required to issue and enforce the API keys every data-plane request needs." -}}
{{- end -}}
{{- if not .Values.admin.imports.hfTokenSecret -}}
{{- fail "admin.imports.hfTokenSecret is required: name an existing Secret (key 'token') holding Pulsys's read-only Hugging Face token. Pulsys uses it for all upstream reads and refuses to start without it." -}}
{{- end -}}
{{- if .Values.admin.enabled -}}
{{- if and (not .Values.postgres.dsn) (not .Values.postgres.existingSecret) (not .Values.postgres.host) -}}
{{- fail "admin.enabled=true requires an external Postgres: set one of postgres.dsn, postgres.existingSecret, or postgres.host" -}}
{{- end -}}
{{- if and (not .Values.postgres.dsn) (not .Values.postgres.existingSecret) .Values.postgres.host (not .Values.postgres.password) -}}
{{- fail "postgres.host is set but postgres.password is empty: set postgres.password, or use postgres.existingSecret / postgres.dsn (preferred for production)" -}}
{{- end -}}
{{- if .Values.oidc.enabled -}}
{{- if not .Values.oidc.issuer -}}
{{- fail "oidc.enabled=true requires oidc.issuer" -}}
{{- end -}}
{{- if not .Values.oidc.redirectURI -}}
{{- fail "oidc.enabled=true requires oidc.redirectURI" -}}
{{- end -}}
{{- if and (not .Values.oidc.clientSecret) (not .Values.oidc.existingSecret) -}}
{{- fail "oidc.enabled=true requires oidc.clientSecret or oidc.existingSecret" -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}
