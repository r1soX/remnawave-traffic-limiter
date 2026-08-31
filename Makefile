.PHONY: help build test clean run docker-build docker-up docker-down

help:
	@echo "Remnawave Traffic Limiter - Available commands"
	@echo ""
	@echo "  make build          Build Go binary"
	@echo "  make test           Run tests"
	@echo "  make test-verbose   Run tests with verbose output"
	@echo "  make test-cover     Run tests with coverage"
	@echo "  make clean          Remove build artifacts"
	@echo "  make run            Run service locally (requires .env)"
	@echo "  make docker-build   Build Docker image"
	@echo "  make docker-up      Start Docker Compose"
	@echo "  make docker-down    Stop Docker Compose"
	@echo "  make docker-logs    Follow Docker logs"
	@echo "  make lint           Run go vet"
	@echo "  make fmt            Format code"

build:
	@mkdir -p bin
	go build -o bin/remnawave-traffic-limiter ./cmd/server
	@echo "Binary built: bin/remnawave-traffic-limiter"

test:
	go test ./...

test-verbose:
	go test -v ./...

test-cover:
	go test -cover ./...

clean:
	rm -rf bin/
	go clean

run: build
	@if [ ! -f .env ]; then echo "Error: .env file not found. Copy .env.example to .env and configure it."; exit 1; fi
	./bin/remnawave-traffic-limiter

docker-build:
	docker build -t remnawave-traffic-limiter:latest .
	@echo "Docker image built: remnawave-traffic-limiter:latest"

docker-up: docker-build
	@if [ ! -f .env ]; then echo "Error: .env file not found. Copy .env.example to .env and configure it."; exit 1; fi
	docker compose up --build

docker-down:
	docker compose down

docker-logs:
	docker compose logs -f

docker-clean:
	docker compose down -v

lint:
	go vet ./...

fmt:
	go fmt ./...

check: lint test
	@echo "All checks passed"

.env:
	cp .env.example .env
	@echo "Created .env from .env.example - please edit it with your configuration"
