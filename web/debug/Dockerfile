FROM nginx:1.30-alpine
# Static Vue page (no build step; Vue is vendored) plus a reverse proxy that adds the API token,
# so the browser never needs it. nginx renders templates/*.template with the env vars below.
COPY default.conf.template /etc/nginx/templates/default.conf.template
COPY index.html app.js style.css /usr/share/nginx/html/
COPY vendor /usr/share/nginx/html/vendor
ENV API_UPSTREAM=http://siteinfo:8080 API_TOKEN=
