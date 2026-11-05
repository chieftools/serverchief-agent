# FORGE CONFIG (DO NOT REMOVE!)
include forge-conf/100002/before/*;
include forge-conf/100002/status.example.com/before/*;

server {
    http2 on;
    listen 443 ssl;
    listen [::]:443 ssl;
    server_name status.example.com;
    server_tokens off;
    root /home/erin/status.example.com/current/public;

    # FORGE SSL (DO NOT REMOVE!)
    ssl_certificate /etc/nginx/ssl/domains/200003/300003/server.crt;
    ssl_certificate_key /etc/nginx/ssl/domains/200003/300003/server.key;

    include forge-conf/100002/site.conf;
}

# FORGE CONFIG (DO NOT REMOVE!)
include forge-conf/100002/after/*;
include forge-conf/100002/status.example.com/after/*;
