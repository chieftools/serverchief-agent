server {
	listen 80;
	listen [::]:80;
	server_name example.com www.example.com;
	return 301 https://$host$request_uri;
}

server {
	listen 443 ssl http2;
	listen [::]:443 ssl http2;
	server_name example.com www.example.com *.cdn.example.com;

	root /var/www/example.com/public/;
	index index.php index.html;

	ssl_certificate /etc/letsencrypt/live/example.com/fullchain.pem;
	ssl_certificate_key /etc/letsencrypt/live/example.com/privkey.pem;

	location / {
		try_files $uri $uri/ /index.php?$query_string;
	}

	location ~ \.php$ {
		include snippets/fastcgi-php.conf;
		fastcgi_pass unix:/run/php/php8.2-fpm.sock;
	}

	location ~ /\.(?!well-known).* {
		deny all;
	}
}
