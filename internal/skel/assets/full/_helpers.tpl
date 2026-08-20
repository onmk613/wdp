{{/* 命名模板：全 chart（含子 chart）可引用 */}}
{{ define "app.fullname" -}}
{{ .app.name }}-{{ .global.env }}
{{- end }}

{{ define "app.labels" -}}
{{ to_yaml (dict "app" .app.name "env" .global.env) | trim }}
{{- end }}
