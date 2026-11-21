# Build context is the repository root (see compose.yaml). The Vue app is
# built with Vite and served by nginx, which also proxies /api/ to DBKeeper.
FROM node:22-alpine AS builder
WORKDIR /src
COPY db-admin/package.json db-admin/package-lock.json* ./
RUN npm install --no-audit --no-fund
COPY db-admin/ ./
RUN npm run build

FROM nginx:1.27-alpine
COPY --from=builder /src/dist /usr/share/nginx/html
COPY db-admin/nginx.conf /etc/nginx/conf.d/default.conf
