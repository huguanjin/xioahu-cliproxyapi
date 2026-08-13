FROM oven/bun:1.3.14 AS frontend-builder

WORKDIR /frontend

COPY Cli-Proxy-API-Management-Center-main/package.json Cli-Proxy-API-Management-Center-main/bun.lock ./
RUN bun install --frozen-lockfile

COPY Cli-Proxy-API-Management-Center-main/ ./
RUN bun run build

FROM golang:1.26-bookworm AS builder

WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends build-essential git && rm -rf /var/lib/apt/lists/*

COPY go.mod go.sum ./

RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

RUN CGO_ENABLED=1 GOOS=linux go build -buildvcs=false -ldflags="-s -w -X 'main.Version=${VERSION}' -X 'main.Commit=${COMMIT}' -X 'main.BuildDate=${BUILD_DATE}'" -o ./CLIProxyAPI ./cmd/server/

FROM debian:bookworm

RUN apt-get update && apt-get install -y --no-install-recommends tzdata ca-certificates && rm -rf /var/lib/apt/lists/*

RUN mkdir /CLIProxyAPI

COPY --from=builder ./app/CLIProxyAPI /CLIProxyAPI/CLIProxyAPI

COPY config.example.yaml /CLIProxyAPI/config.example.yaml

# Bundled management control panel, built from our own
# Cli-Proxy-API-Management-Center-main/ fork instead of downloaded from
# upstream at runtime. Served via MANAGEMENT_STATIC_PATH (docker-compose.yml).
COPY --from=frontend-builder /frontend/dist/index.html /CLIProxyAPI/static/management.html

WORKDIR /CLIProxyAPI

EXPOSE 8317

ENV TZ=Asia/Shanghai

RUN cp /usr/share/zoneinfo/${TZ} /etc/localtime && echo "${TZ}" > /etc/timezone

CMD ["./CLIProxyAPI"]
