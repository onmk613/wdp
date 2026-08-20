{{/* 命名模板：全 chart 可引用（include 可参与管道拼接） */}}
{{ define "app.fullname" -}}
{{ .app.name }}-{{ .global.env }}
{{- end }}
