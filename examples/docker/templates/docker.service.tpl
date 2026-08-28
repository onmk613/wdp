[Unit]
Description=Docker Application Container Engine
Documentation=https://docs.docker.com
After=network-online.target time-set.target
Wants=network-online.target
StartLimitBurst=3
StartLimitIntervalSec=60

[Service]
Type=notify
ExecStart={{ .bin_dir }}/dockerd -H unix:///var/run/docker.sock
ExecReload=/bin/kill -s HUP $MAINPID
TimeoutStartSec=0
Restart=always
RestartSec=2

# 与 ansible 参考单元对齐的资源/进程治理：Delegate 交给 dockerd 自管容器 cgroup，
# KillMode=process 避免 systemd 重启 dockerd 时连带杀掉容器。
LimitNOFILE=1048576
LimitNPROC=infinity
LimitCORE=infinity
TasksMax=infinity
Delegate=yes
KillMode=process
OOMScoreAdjust=-500

[Install]
WantedBy=multi-user.target
