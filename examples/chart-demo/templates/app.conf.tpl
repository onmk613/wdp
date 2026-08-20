# 渲染自 templates/app.conf.tpl
fullname = {{ include "app.fullname" . }}
home     = {{ include "app.home" . }}
port     = {{ .app.port }}
replicas = {{ .app.replicas }}
debug    = {{ .app.debug }}
env      = {{ .global.env }}
host     = {{ .inventory_hostname }}
jdk      = {{ .jdk.version }}
