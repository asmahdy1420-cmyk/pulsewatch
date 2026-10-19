BINARY  := bin/pulsewatch
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build run test race vet fmt fmt-check cover docker clean

all: fmt-check vet test build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/pulsewatch

run: build
	./$(BINARY) -config pulsewatch.example.yaml

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

docker:
	docker build --build-arg VERSION=$(VERSION) -t pulsewatch:$(VERSION) .

clean:
	rm -rf bin coverage.out
