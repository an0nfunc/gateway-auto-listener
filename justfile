image := env("IMAGE", "ghcr.io/an0nfunc/gateway-auto-listener")
version := `git describe --tags --always --dirty 2>/dev/null || echo "dev"`

default:
    @just --list

# Build the controller binary
build:
    go build -buildmode=pie -trimpath -ldflags="-s -w -X main.version={{ version }}" -o bin/gateway-auto-listener ./cmd/gateway-auto-listener

# Build Docker image
docker-build:
    docker build --build-arg VERSION={{ version }} -t {{ image }}:{{ version }} -t {{ image }}:latest .

# Push Docker image
docker-push:
    docker push {{ image }}:{{ version }}
    docker push {{ image }}:latest

# Run tests
test:
    go test ./...

# Run locally (requires kubeconfig)
run *ARGS:
    go run ./cmd/gateway-auto-listener {{ ARGS }}

# Download and tidy dependencies
deps:
    go mod download
    go mod tidy

# Run linter
lint:
    golangci-lint run ./...

# Run go vet
vet:
    go vet ./...
