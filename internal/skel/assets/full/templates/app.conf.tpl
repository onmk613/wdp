# 渲染自 templates/app.conf.tpl（include + sprig 函数演示）
fullname = {{ include "app.fullname" . }}
env      = {{ .global.env }}
host     = {{ .inventory_hostname }}
replicas = {{ .app.replicas }}
labels   = {{ include "app.labels" . | indent 2 }}
issued   = {{ now | date "2006-01-02" }}
