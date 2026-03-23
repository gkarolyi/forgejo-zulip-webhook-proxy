FROM golang:1.23-alpine AS builder
WORKDIR /app
COPY go.mod .
COPY *.go .
RUN go test ./...
RUN go build -ldflags="-s -w" -o proxy .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /app/proxy .
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
    CMD wget -qO- http://localhost:8080/health || exit 1
CMD ["./proxy"]
