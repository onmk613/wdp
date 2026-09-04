version = 2
root = "{{ dig "docker_data_dir" (printf "%s/containerd" .data_dir) . }}"
state = "/run/containerd"
