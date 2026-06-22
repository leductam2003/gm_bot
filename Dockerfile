# Build stage — pure-Go, no CGO so the binary is fully static.
FROM golang:1.21-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/zyper-bot ./cmd/server

# Runtime stage — minimal.
FROM alpine:3.19
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 zyper
WORKDIR /app
COPY --from=build /out/zyper-bot /app/zyper-bot
COPY web /app/web
USER zyper
ENV ZYPER_ADDR=0.0.0.0:8787 ZYPER_DB=/data/zyperbot.db ZYPER_WEB=/app/web
VOLUME ["/data"]
EXPOSE 8787
ENTRYPOINT ["/app/zyper-bot"]
