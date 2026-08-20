{{/* nginx 子 chart 自有命名模板（与父 helpers 合并注册，全局可引用） */}}

{{/* upstream 名：应用名-环境（父作用域键经 global 传递，子 chart 看不到 app.*） */}}
{{ define "nginx.upstream_name" -}}
{{ .global.env }}-backend
{{- end }}
