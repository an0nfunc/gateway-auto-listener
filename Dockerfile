FROM golang:1-alpine AS builder
RUN apk add --no-cache build-base
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN go build -buildmode=pie -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o gateway-auto-listener ./cmd/gateway-auto-listener

FROM alpine:latest
RUN apk add --no-cache ca-certificates
WORKDIR /
COPY --from=builder /app/gateway-auto-listener .
USER 65532:65532
ENTRYPOINT ["/gateway-auto-listener"]
