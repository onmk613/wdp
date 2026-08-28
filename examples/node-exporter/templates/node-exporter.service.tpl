[Unit]
Description=Node Exporter
Documentation=https://github.com/prometheus/node_exporter
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User={{ .user }}
ExecStart={{ .bin_dir }}/node_exporter \
  --collector.systemd \
  --collector.processes \
{{- if dig "node_exporter_smart" .smart . }}
  --collector.textfile.directory={{ .collector_textfile_directory }} \
{{- end }}
  --web.listen-address=:{{ dig "node_exporter_port" .port . }}
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
