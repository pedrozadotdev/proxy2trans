FROM golang:1.25-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY main.go ./

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/proxy2trans \
    .

FROM alpine:3.22

RUN apk add --no-cache \
    iproute2 \
    nftables \
    ca-certificates

COPY --from=builder /out/proxy2trans /usr/local/bin/proxy2trans
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
COPY config.yml /etc/proxy2trans/config.yml

RUN chmod +x /usr/local/bin/entrypoint.sh \
    && mkdir -p /etc/proxy2trans

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["/etc/proxy2trans/config.yml"]
