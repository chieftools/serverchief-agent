# FORGE CONFIG (DO NOT REMOVE!)
include forge-conf/2937366/before/*;
include forge-conf/2937366/afspraak.mkin.nl/before/*;

server {
    http2 on;
    listen 443 ssl;
    listen [::]:443 ssl;
    server_name afspraak.mkin.nl;
    server_tokens off;
    root /home/mkin/afspraak.mkin.nl/current/public;

    # FORGE SSL (DO NOT REMOVE!)
    ssl_certificate /etc/nginx/ssl/domains/1937544/3714827/server.crt;
    ssl_certificate_key /etc/nginx/ssl/domains/1937544/3714827/server.key;

    include forge-conf/2937366/site.conf;
}

# FORGE CONFIG (DO NOT REMOVE!)
include forge-conf/2937366/after/*;
include forge-conf/2937366/afspraak.mkin.nl/after/*;
