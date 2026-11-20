# llm-relay — multi-stage build: static Go binary on a minimal runtime.
# No `# syntax=` directive on purpose: it forces a network fetch of the BuildKit
# frontend image and breaks offline builds. The built-in frontend is enough.

# ---------- web dashboard ----------
FROM node:24-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

# ---------- go binary ----------
FROM golang:1.26 AS build
WORKDIR /src

# Dependency layer first so code edits do not invalidate the module cache.
# go.sum is globbed on purpose: it does not exist until the first external
# dependency is added, and a hard COPY of a missing file fails the whole build.
# (go.mod itself must still exist — create it in the first implementation commit.)
COPY go.mod go.sum* ./
RUN go mod download

COPY . .

# Fresh SPA build from the web stage wins over whatever dist was committed.
COPY --from=web /web/dist ./web/dist

# CGO off => fully static binary => tiny final image.
ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath -ldflags="-s -w" -o /out/llm-relay ./cmd/llm-relay

# ---------- runtime ----------
FROM scratch

COPY --from=build /out/llm-relay /llm-relay

# CA certs: needed for any outbound TLS the service makes.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Timezone database. `scratch` ships none, so TZ=Europe/Helsinki from
# docker-compose silently falls back to UTC and every timestamp the service
# writes is wrong, with no error raised. Leaner alternative once Go code exists:
# `import _ "time/tzdata"` in main (~450 KB instead of ~4 MB) — then delete
# this line.
COPY --from=build /usr/share/zoneinfo /usr/share/zoneinfo

EXPOSE 8080
EXPOSE 8081

# NOTE: `scratch` has no /bin/sh and no /bin/bash, so `make docker-shell` cannot
# work against this image. If you need an exec shell or a container HEALTHCHECK,
# switch the runtime stage to gcr.io/distroless/base (~2 MB) — that resolves both.
ENTRYPOINT ["/llm-relay"]
