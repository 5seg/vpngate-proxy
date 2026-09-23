FROM golang:alpine AS builder
WORKDIR /build

COPY go.mod ./
COPY main.go ./

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o vpngate-proxy .

FROM alpine:latest

RUN apk add --no-cache openvpn iproute2 ca-certificates

WORKDIR /app
COPY --from=builder /build/vpngate-proxy /app/vpngate-proxy
COPY --chmod=755 entrypoint.sh /app/entrypoint.sh

VOLUME ["/data"]

EXPOSE 1080 8080

ENTRYPOINT ["/app/entrypoint.sh"]
