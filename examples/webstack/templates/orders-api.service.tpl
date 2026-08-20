[Unit]
Description={{ include "webstack.fullname" . }} {{ .app.version }} (managed by wdp)
After=network.target

[Service]
Type=simple
User={{ .common.user }}
Group={{ .common.group }}
WorkingDirectory={{ include "webstack.current" . }}
ExecStart={{ include "webstack.current" . }}/bin/orders-api -config {{ .global.workdir }}/shared/app.conf
Environment=APP_ENV={{ .global.env }}
Environment=LOG_LEVEL={{ .app.log.level }}
Restart=on-failure
RestartSec=3

[Install]
WantedBy=multi-user.target
