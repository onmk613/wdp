{
  "data-root": "{{ dig "docker_data_dir" .data_dir . }}/docker",
  "log-driver": "{{ .log.driver }}",
  "log-level": "{{ .log.level }}",
  "log-opts": {
    "max-size": "{{ .log.max_size }}",
    "max-file": "{{ .log.max_file | toString }}"
  },
  "iptables": true,
  "userland-proxy": false
}
