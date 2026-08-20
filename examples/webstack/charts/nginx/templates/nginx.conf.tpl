# nginx 反向代理配置 —— 渲染自 charts/nginx/templates/nginx.conf.tpl
# 亮点：upstream 成员由内置变量 .groups/.hosts 自动发现——
#   appservers 组增减主机无需改配置，重新部署即收敛
worker_processes {{ .workers }};

events {
    worker_connections 1024;
}

http {
    keepalive_timeout  {{ .keepalive }};
    client_max_body_size {{ .client_max_body }};

    upstream {{ include "nginx.upstream_name" . }} {
{{- range $name := .groups.appservers }}
        server {{ (index $.hosts $name).address }}:{{ $.global.app_port }};
{{- end }}
    }

    server {
        listen {{ .listen_port }};
        server_name {{ .global.domain }};

        location / {
            proxy_pass http://{{ include "nginx.upstream_name" . }};
            proxy_set_header Host $host;
            proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        }

        location /healthz {
            access_log off;
            return 200 "ok\n";
        }
    }
}
