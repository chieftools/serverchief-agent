# FORGE CONFIG (DO NOT REMOVE!)
include forge-conf/100001/before/*;
include forge-conf/100001/booking.example.com/before/*;

server {
    http2 on;
    listen 443 ssl;
    listen [::]:443 ssl;
    server_name booking.example.com;
    server_tokens off;
    root /home/dave/booking.example.com/current/public;

    # FORGE SSL (DO NOT REMOVE!)
    ssl_certificate /etc/nginx/ssl/domains/200001/300001/server.crt;
    ssl_certificate_key /etc/nginx/ssl/domains/200001/300001/server.key;

    include forge-conf/100001/site.conf;
}

# FORGE CONFIG (DO NOT REMOVE!)
include forge-conf/100001/after/*;
include forge-conf/100001/booking.example.com/after/*;
