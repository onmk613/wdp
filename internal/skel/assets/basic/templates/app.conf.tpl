# 渲染自 templates/app.conf.tpl
fullname = {{ include "app.fullname" . }}
port     = {{ .app.port }}
env      = {{ .global.env }}
host     = {{ .inventory_hostname }}
