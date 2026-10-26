# Ploi Webserver Configuration, do not remove!
include /etc/nginx/ploi/backup.chief.app/before/*;

server {
    #listen 80;
    #listen [::]:80;

    root /home/backupchief/backup.chief.app/production/current/public;
    server_name backup.chief.app;

    include /etc/nginx/ssl/backup.chief.app;

    index index.php;

    # Ploi Configuration, do not remove!
    include /etc/nginx/ploi/backup.chief.app/server/*;

    location / {
        # Remove any leading index.php/index.html/index.htm from the request url
        rewrite ^(.*/)index\.(php|html|htm)$ $scheme://$host$1 permanent;
        try_files $uri /index.php?$query_string;
    }

    location = /index.php {
        include fastcgi_params;
        fastcgi_pass unix:/run/php/php8.5-fpm-backupchief.sock;
    }
}

# Ploi Webserver Configuration, do not remove!
include /etc/nginx/ploi/backup.chief.app/after/*;
