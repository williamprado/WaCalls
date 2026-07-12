# syntax=docker/dockerfile:1
#
# Imagem própria da WaCalls (williamwilmer10/wacalls). Build 100% puro-Go
# (CGO_ENABLED=0): codec MLow e driver SQLite (modernc) são Go puro, então NÃO
# precisamos do libopus_mlow.so em C nem de gcc — binário estático, roda em
# qualquer x86-64.

# ---------- Stage 1: build do client React ----------
FROM node:22-alpine AS client
WORKDIR /app/client
COPY client/package*.json ./
RUN npm ci
COPY client/ ./
RUN npm run build

# ---------- Stage 2: build do servidor Go (estático, puro-Go) ----------
FROM golang:1.26-alpine AS server
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /wacalls ./cmd/server

# ---------- Stage 3: runtime mínimo ----------
FROM alpine:3.20
RUN apk add --no-cache ca-certificates wget \
    && adduser -D -u 10001 app \
    && mkdir -p /data && chown app /data
WORKDIR /app
COPY --from=server /wacalls /app/wacalls
COPY --from=client /app/client/dist /app/client/dist
USER app
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=15s --timeout=5s --start-period=15s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/ >/dev/null 2>&1 || exit 1
ENTRYPOINT ["/app/wacalls"]
CMD ["-addr", ":8080", "-static", "/app/client/dist", "-db", "/data/wacalls.db"]
