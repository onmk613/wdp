{{/* 子 chart 自己的命名模板（与父 helpers 合并注册，全局可引用） */}}
{{ define "jdk.pkg" -}}
jdk-{{ .version }}_{{ .global.env }}
{{- end }}
