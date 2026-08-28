[Unit]
Description=Run node_exporter SMART collector every {{ .smart_check_interval }} minutes

[Timer]
OnCalendar=*:0/{{ .smart_check_interval }}
RandomizedDelaySec=30
Persistent=true

[Install]
WantedBy=timers.target
