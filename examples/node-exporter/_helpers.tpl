{{/* node-exporter chart 公共命名模板 */}}

{{/* 架构别名：下载 URL 用 Go 风格（x86_64→amd64，aarch64→arm64）；不支持 → unsupported */}}
{{ define "node-exporter.arch_alias" -}}
{{ index (dict "x86_64" "amd64" "aarch64" "arm64" "amd64" "amd64" "arm64" "arm64") .arch | default "unsupported" }}
{{- end }}
