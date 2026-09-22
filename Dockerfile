# syntax=docker/dockerfile:1
FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/verdent-server ./cmd/server && \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/verdent-login ./cmd/login

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && adduser -D -h /app app
WORKDIR /app
COPY --from=builder /out/verdent-server /usr/local/bin/
COPY --from=builder /out/verdent-login /usr/local/bin/
COPY config.example.json /app/config.json
USER app
EXPOSE 7866
VOLUME ["/app/auths", "/app/data"]
ENTRYPOINT ["verdent-server", "-config", "/app/config.json"]
