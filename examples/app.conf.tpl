# 由 wdp template 模块渲染
app_name = {{ .app_name }}
port = {{ .nginx_port }}
host = {{ .inventory_hostname }}
os = {{ .os.name }} {{ .os.version }} ({{ .arch }})
