FROM golang:1.23-alpine AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./
COPY webui ./webui

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -trimpath -o /Freebuff2API .

FROM metacubex/mihomo:v1.19.22

RUN mkdir -p /data

COPY --from=builder /Freebuff2API /usr/local/bin/Freebuff2API
ENV DATA_DIR=/data
ENV EMBEDDED_MIHOMO_BINARY_PATH=/usr/local/bin/mihomo
VOLUME ["/data"]
EXPOSE 8080 7897 9097

ENTRYPOINT ["/usr/local/bin/Freebuff2API"]
