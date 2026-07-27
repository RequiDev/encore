# Encore web client: a static React bundle served by nginx, which also proxies
# /api to the API container so the browser sees a single origin. That is what
# lets the session cookie be SameSite=Lax with no CORS configuration at all.

# --- build -----------------------------------------------------------------
# The output is static files, so this stage always runs natively whatever
# architecture the final image targets. Running npm under QEMU would be slow
# and would produce byte-identical output.
FROM --platform=$BUILDPLATFORM node:22-alpine AS build

WORKDIR /app

COPY web/package.json web/package-lock.json ./
RUN npm ci

COPY web/ ./
# The client talks to a relative /api path, so no build-time API URL is needed.
RUN npm run build

# --- runtime ---------------------------------------------------------------
FROM nginx:1.29-alpine AS runtime

RUN rm -rf /usr/share/nginx/html/*
COPY --from=build /app/dist /usr/share/nginx/html
COPY deploy/nginx.conf /etc/nginx/conf.d/default.conf

# nginx's own healthcheck endpoint, so compose can wait for the UI as well.
RUN printf 'ok\n' > /usr/share/nginx/html/healthz.txt

EXPOSE 80
