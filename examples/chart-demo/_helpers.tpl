{{/* 命名模板：全局可引用（include 可参与管道拼接） */}}
{{ define "app.fullname" -}}
{{ .app.name }}-{{ .global.env }}
{{- end }}

{{ define "app.home" -}}
{{ .global.workdir }}/{{ include "app.fullname" . }}
{{- end }}
