# orders-api 应用配置 —— 由 wdp template 模块渲染
# 渲染域：合并后 values（app/nginx/common/global…）+ 强制内置变量 + facts
#
# 演示的模板能力：include 命名模板 / range 循环 / to_yaml 结构化输出 /
# b64enc 敏感值编码 / sprig 函数（now、default、quote）/ 条件分支

fullname = {{ include "webstack.fullname" . }}
release  = {{ .app.version }}
env      = {{ .global.env }}
host     = {{ .inventory_hostname }}
deployed = {{ now | date "2006-01-02 15:04:05" }}

[server]
port     = {{ .app.port }}
replicas = {{ .app.replicas }}

[log]
level      = {{ .app.log.level | quote }}
dir        = {{ .app.log.dir }}
rotate_mb  = {{ .app.log.rotate_mb }}

[features]
metrics = {{ .app.features.metrics }}
tracing = {{ .app.features.tracing }}
{{/* 嵌套 map 整体 to_yaml 输出并缩进 */}}
[features.cache]
{{ to_yaml .app.features.cache | indent 2 }}

[secrets]
{{/* 生产经 --set 注入；配置文件侧仅存编码态 */}}
api_token_b64 = {{ .app.secrets.api_token | b64enc }}

[monitoring]
{{ if .monitoring.enabled }}
enabled  = true
endpoint = {{ .monitoring.endpoint | quote }}
{{ else }}
enabled  = false
{{ end }}

[routes]
{{ range .app.endpoints }}
route.{{ . }} = /api/v1/{{ . }}
{{ end }}
route.healthz = {{ include "webstack.health_url" . }}
