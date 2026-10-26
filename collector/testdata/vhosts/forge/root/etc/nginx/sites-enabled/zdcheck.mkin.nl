# FORGE CONFIG (DO NOT REMOVE!)
include forge-conf/2939165/before/*;
include forge-conf/2939165/zdcheck.mkin.nl/before/*;

server {
    http2 on;
    listen 443 ssl;
    listen [::]:443 ssl;
    server_name zdcheck.mkin.nl;
    server_tokens off;
    root /home/afspraak/zdcheck.mkin.nl/current/public;

    # FORGE SSL (DO NOT REMOVE!)
    ssl_certificate /etc/nginx/ssl/domains/1037519/2979440/server.crt;
    ssl_certificate_key /etc/nginx/ssl/domains/1037519/2979440/server.key;

    include forge-conf/2939165/site.conf;
}

# FORGE CONFIG (DO NOT REMOVE!)
include forge-conf/2939165/after/*;
include forge-conf/2939165/zdcheck.mkin.nl/after/*;
