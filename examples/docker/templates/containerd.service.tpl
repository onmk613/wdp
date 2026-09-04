[Unit]
Description=containerd container runtime
Documentation=https://containerd.io
After=network.target

[Service]
User=root
ExecStartPre=/sbin/modprobe overlay
ExecStart={{ .bin_dir }}/containerd

Type=notify
Restart=always
RestartSec=5
Delegate=yes
KillMode=process

LimitNOFILE=1048576
LimitNPROC=infinity
LimitCORE=infinity

TasksMax=infinity
OOMScoreAdjust=-999

[Install]
WantedBy=multi-user.target