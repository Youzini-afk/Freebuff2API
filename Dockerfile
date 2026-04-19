FROM golang:1.23-alpine AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./
COPY webui ./webui

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -trimpath -o /Freebuff2API .

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata \
    && mkdir -p /data

COPY --from=builder /Freebuff2API /usr/local/bin/Freebuff2API
ENV DATA_DIR=/data
VOLUME ["/data"]
# Expose proxy port
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/Freebuff2API"]
