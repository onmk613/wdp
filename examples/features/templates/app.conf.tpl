# template 模块演示：本地渲染后分发（幂等同 copy）
# 渲染域 = play vars + 主机/组变量 + 强制内置变量 + facts
app = {{ .demo_app }}
port = {{ .demo_port }}
host = {{ .inventory_hostname }}
os = {{ if .os }}{{ .os.name }} {{ .os.version }}{{ else }}unknown{{ end }}
members = {{ len .play_hosts }}
