# FORGE CONFIG (DO NOT REMOVE!)
include forge-conf/2937366/before/*;
include forge-conf/2937366/admin.zdcheck.mkin.nl/before/*;

server {
    http2 on;
    listen 443 ssl;
    listen [::]:443 ssl;
    server_name admin.zdcheck.mkin.nl;
    server_tokens off;
    root /home/mkin/admin.zdcheck.mkin.nl/current/public;

    # FORGE SSL (DO NOT REMOVE!)
    ssl_certificate /etc/nginx/ssl/domains/1037424/2979372/server.crt;
    ssl_certificate_key /etc/nginx/ssl/domains/1037424/2979372/server.key;

    include forge-conf/2937366/site.conf;
}

# FORGE CONFIG (DO NOT REMOVE!)
include forge-conf/2937366/after/*;
include forge-conf/2937366/admin.zdcheck.mkin.nl/after/*;
