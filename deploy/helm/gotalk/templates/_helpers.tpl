{{/* Expand the name of the chart. */}}
{{- define "gotalk.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "gotalk.fullname" -}}
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

{{- define "gotalk.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" -}}
{{- end -}}

{{- define "gotalk.labels" -}}
helm.sh/chart: {{ include "gotalk.chart" . }}
app.kubernetes.io/name: {{ include "gotalk.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "gotalk.selectorLabels" -}}
app.kubernetes.io/name: {{ include "gotalk.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "gotalk.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "gotalk.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "gotalk.secretName" -}}
{{- default (include "gotalk.fullname" .) .Values.existingSecret -}}
{{- end -}}

{{- define "gotalk.image" -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}

{{- define "gotalk.usesManagedSecret" -}}
{{- if not .Values.existingSecret -}}
{{- if or .Values.database.url .Values.redis.url .Values.auth.jwtSecret .Values.setup.admin.password .Values.storage.s3.accessKeyId .Values.storage.s3.secretAccessKey .Values.mail.smtp.password .Values.mail.apiKey .Values.mail.accessKeyId .Values.mail.secretAccessKey .Values.voice.livekitApiKey .Values.voice.livekitApiSecret .Values.postgresql.enabled -}}true{{- end -}}
{{- end -}}
{{- end -}}

{{- define "gotalk.validate" -}}
{{- $dbConfigured := or .Values.database.url .Values.database.existingSecret.name .Values.postgresql.enabled -}}
{{- if not $dbConfigured -}}
{{- fail "Gotalk requires PostgreSQL: set database.url, database.existingSecret.name, or postgresql.enabled=true." -}}
{{- end -}}
{{- $rwx := false -}}
{{- range .Values.persistence.accessModes -}}
{{- if eq . "ReadWriteMany" -}}{{- $rwx = true -}}{{- end -}}
{{- end -}}
{{- $multi := or (gt (int .Values.replicaCount) 1) (and .Values.autoscaling.enabled (gt (int .Values.autoscaling.maxReplicas) 1)) -}}
{{- if and (eq (default "local" .Values.storage.driver) "local") $multi (not $rwx) -}}
{{- fail "Gotalk local storage is not safe with multiple replicas or autoscaling unless persistence.accessModes contains ReadWriteMany; use storage.driver=s3 or a RWX PVC." -}}
{{- end -}}
{{- $redisConfigured := or .Values.redis.url .Values.redis.existingSecret.name .Values.redis.enabled -}}
{{- if and $multi (not $redisConfigured) -}}
{{- fail "Gotalk requires Redis for multiple replicas or autoscaling: set redis.url, redis.existingSecret.name, or redis.enabled=true." -}}
{{- end -}}
{{- end -}}

{{- define "gotalk.envFrom" -}}
{{- if .Values.existingSecret }}
- secretRef:
    name: {{ .Values.existingSecret | quote }}
{{- else if include "gotalk.usesManagedSecret" . }}
- secretRef:
    name: {{ include "gotalk.secretName" . }}
{{- end }}
{{- with .Values.extraEnvFrom }}
{{- toYaml . }}
{{- end }}
{{- end -}}

{{- define "gotalk.env" -}}
- name: GOTALK_SERVER_PUBLIC_URL
  value: {{ .Values.server.publicUrl | quote }}
- name: GOTALK_SERVER_TRUST_PROXY
  value: {{ .Values.server.trustProxy | quote }}
{{- if .Values.server.trustedProxies }}
- name: GOTALK_SERVER_TRUSTED_PROXIES
  value: {{ .Values.server.trustedProxies | quote }}
{{- end }}
{{- if .Values.server.corsAllowedOrigins }}
- name: GOTALK_SERVER_CORS_ALLOWED_ORIGINS
  value: {{ .Values.server.corsAllowedOrigins | quote }}
{{- end }}
- name: GOTALK_LOG_FORMAT
  value: {{ .Values.log.format | quote }}
- name: GOTALK_LOG_LEVEL
  value: {{ .Values.log.level | quote }}
- name: GOTALK_SETUP_INSTANCE_NAME
  value: {{ .Values.setup.instanceName | quote }}
- name: GOTALK_SETUP_REGISTRATION_MODE
  value: {{ .Values.setup.registrationMode | quote }}
{{- if .Values.setup.admin.username }}
- name: GOTALK_SETUP_ADMIN_USERNAME
  value: {{ .Values.setup.admin.username | quote }}
{{- end }}
{{- if .Values.setup.admin.email }}
- name: GOTALK_SETUP_ADMIN_EMAIL
  value: {{ .Values.setup.admin.email | quote }}
{{- end }}
{{- if .Values.setup.admin.existingSecret.name }}
- name: GOTALK_SETUP_ADMIN_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ .Values.setup.admin.existingSecret.name | quote }}
      key: {{ .Values.setup.admin.existingSecret.passwordKey | quote }}
{{- end }}
{{- if .Values.postgresql.enabled }}
- name: GOTALK_DATABASE_URL
  value: {{ printf "host=%s-postgresql port=5432 user=%s dbname=%s sslmode=disable" (include "gotalk.fullname" .) .Values.postgresql.auth.username .Values.postgresql.auth.database | quote }}
- name: PGPASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ include "gotalk.secretName" . }}
      key: PGPASSWORD
{{- else if .Values.database.existingSecret.name }}
- name: GOTALK_DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ .Values.database.existingSecret.name | quote }}
      key: {{ .Values.database.existingSecret.key | quote }}
{{- end }}
{{- if .Values.redis.enabled }}
- name: GOTALK_REDIS_URL
  value: {{ printf "redis://%s-redis:6379/0" (include "gotalk.fullname" .) | quote }}
{{- else if .Values.redis.existingSecret.name }}
- name: GOTALK_REDIS_URL
  valueFrom:
    secretKeyRef:
      name: {{ .Values.redis.existingSecret.name | quote }}
      key: {{ .Values.redis.existingSecret.key | quote }}
{{- end }}
{{- if .Values.auth.existingSecret.name }}
- name: GOTALK_AUTH_JWT_SECRET
  valueFrom:
    secretKeyRef:
      name: {{ .Values.auth.existingSecret.name | quote }}
      key: {{ .Values.auth.existingSecret.key | quote }}
{{- end }}
- name: GOTALK_STORAGE_DRIVER
  value: {{ default "local" .Values.storage.driver | quote }}
- name: GOTALK_STORAGE_LOCAL_PATH
  value: {{ .Values.storage.local.path | quote }}
{{- if .Values.storage.publicUrl }}
- name: GOTALK_STORAGE_PUBLIC_URL
  value: {{ .Values.storage.publicUrl | quote }}
{{- end }}
{{- if .Values.storage.uploadsMaxSize }}
- name: GOTALK_UPLOADS_MAX_SIZE
  value: {{ .Values.storage.uploadsMaxSize | quote }}
{{- end }}
{{- if .Values.storage.s3.endpoint }}
- name: GOTALK_STORAGE_S3_ENDPOINT
  value: {{ .Values.storage.s3.endpoint | quote }}
{{- end }}
{{- if .Values.storage.s3.region }}
- name: GOTALK_STORAGE_S3_REGION
  value: {{ .Values.storage.s3.region | quote }}
{{- end }}
{{- if .Values.storage.s3.bucket }}
- name: GOTALK_STORAGE_S3_BUCKET
  value: {{ .Values.storage.s3.bucket | quote }}
{{- end }}
{{- if .Values.storage.s3.prefix }}
- name: GOTALK_STORAGE_S3_PREFIX
  value: {{ .Values.storage.s3.prefix | quote }}
{{- end }}
{{- if .Values.storage.s3.forcePathStyle }}
- name: GOTALK_STORAGE_S3_FORCE_PATH_STYLE
  value: {{ .Values.storage.s3.forcePathStyle | quote }}
{{- end }}
{{- if .Values.storage.s3.existingSecret.name }}
- name: GOTALK_STORAGE_S3_ACCESS_KEY_ID
  valueFrom:
    secretKeyRef:
      name: {{ .Values.storage.s3.existingSecret.name | quote }}
      key: {{ .Values.storage.s3.existingSecret.accessKeyIdKey | quote }}
- name: GOTALK_STORAGE_S3_SECRET_ACCESS_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .Values.storage.s3.existingSecret.name | quote }}
      key: {{ .Values.storage.s3.existingSecret.secretAccessKeyKey | quote }}
{{- end }}
{{- if .Values.mail.driver }}
- name: GOTALK_MAIL_DRIVER
  value: {{ .Values.mail.driver | quote }}
{{- end }}
{{- if .Values.mail.from }}
- name: GOTALK_MAIL_FROM
  value: {{ .Values.mail.from | quote }}
{{- end }}
{{- if .Values.mail.smtp.host }}
- name: GOTALK_MAIL_SMTP_HOST
  value: {{ .Values.mail.smtp.host | quote }}
{{- end }}
{{- if .Values.mail.smtp.port }}
- name: GOTALK_MAIL_SMTP_PORT
  value: {{ .Values.mail.smtp.port | quote }}
{{- end }}
{{- if .Values.mail.smtp.username }}
- name: GOTALK_MAIL_SMTP_USERNAME
  value: {{ .Values.mail.smtp.username | quote }}
{{- end }}
{{- if .Values.mail.smtp.tls }}
- name: GOTALK_MAIL_SMTP_TLS
  value: {{ .Values.mail.smtp.tls | quote }}
{{- end }}
{{- if .Values.mail.apiUrl }}
- name: GOTALK_MAIL_API_URL
  value: {{ .Values.mail.apiUrl | quote }}
{{- end }}
{{- if .Values.mail.domain }}
- name: GOTALK_MAIL_DOMAIN
  value: {{ .Values.mail.domain | quote }}
{{- end }}
{{- if .Values.mail.region }}
- name: GOTALK_MAIL_REGION
  value: {{ .Values.mail.region | quote }}
{{- end }}
{{- if .Values.mail.existingSecret.name }}
- name: GOTALK_MAIL_SMTP_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ .Values.mail.existingSecret.name | quote }}
      key: {{ .Values.mail.existingSecret.passwordKey | quote }}
- name: GOTALK_MAIL_API_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .Values.mail.existingSecret.name | quote }}
      key: {{ .Values.mail.existingSecret.apiKeyKey | quote }}
- name: GOTALK_MAIL_ACCESS_KEY_ID
  valueFrom:
    secretKeyRef:
      name: {{ .Values.mail.existingSecret.name | quote }}
      key: {{ .Values.mail.existingSecret.accessKeyIdKey | quote }}
- name: GOTALK_MAIL_SECRET_ACCESS_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .Values.mail.existingSecret.name | quote }}
      key: {{ .Values.mail.existingSecret.secretAccessKeyKey | quote }}
{{- end }}
{{- if .Values.voice.livekitUrl }}
- name: GOTALK_VOICE_LIVEKIT_URL
  value: {{ .Values.voice.livekitUrl | quote }}
{{- end }}
{{- if .Values.voice.livekitApiUrl }}
- name: GOTALK_VOICE_LIVEKIT_API_URL
  value: {{ .Values.voice.livekitApiUrl | quote }}
{{- end }}
{{- if .Values.voice.existingSecret.name }}
- name: GOTALK_VOICE_LIVEKIT_API_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .Values.voice.existingSecret.name | quote }}
      key: {{ .Values.voice.existingSecret.apiKeyKey | quote }}
- name: GOTALK_VOICE_LIVEKIT_API_SECRET
  valueFrom:
    secretKeyRef:
      name: {{ .Values.voice.existingSecret.name | quote }}
      key: {{ .Values.voice.existingSecret.apiSecretKey | quote }}
{{- end }}
{{- with .Values.extraEnv }}
{{- toYaml . }}
{{- end }}
{{- end -}}
