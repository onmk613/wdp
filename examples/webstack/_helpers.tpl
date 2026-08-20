{{/* 全 chart（含子 chart）可引用的命名模板 */}}

{{/* 资源全名：应用名-环境 */}}
{{ define "webstack.fullname" -}}
{{ .app.name }}-{{ .global.env }}
{{- end }}

{{/* 标准标签（to_yaml + indent 的惯用组合） */}}
{{ define "webstack.labels" -}}
{{ to_yaml (dict "app" .app.name "env" .global.env "chart" "webstack") | indent 2 }}
{{- end }}

{{/* 当前版本目录与发布路径 */}}
{{ define "webstack.release_dir" -}}
{{ .global.workdir }}/releases/{{ .app.version }}
{{- end }}

{{ define "webstack.current" -}}
{{ .global.workdir }}/current
{{- end }}

{{/* 健康端点：tracing 开启时附加 zipkin 上报地址 */}}
{{ define "webstack.health_url" -}}
http://localhost:{{ .app.port }}/healthz{{ if .app.features.tracing }}?trace=1{{ end }}
{{- end }}
