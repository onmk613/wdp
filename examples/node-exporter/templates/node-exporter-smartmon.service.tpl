[Unit]
Description=node_exporter SMART textfile collector (smartmon.sh)
Documentation=https://github.com/prometheus/node_exporter/tree/master/text_collector_examples

[Service]
Type=oneshot
# 临时文件 + mv 原子落盘，node_exporter 不会读到半截指标（替代 ansible 版的 sponge）
ExecStart=/bin/sh -c '{{ .bin_dir }}/smartmon.sh > {{ .collector_textfile_directory }}/smartmon.prom.tmp && mv {{ .collector_textfile_directory }}/smartmon.prom.tmp {{ .collector_textfile_directory }}/smartmon.prom'
