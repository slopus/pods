VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
BIN     := bin

.PHONY: build test run docker clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN)/podbay ./cmd/podbay
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN)/pods ./cmd/pods

test:
	go test ./...

run:
	go run -ldflags '$(LDFLAGS)' ./cmd/podbay

docker:
	docker build --build-arg VERSION=$(VERSION) -t podbay:$(VERSION) -t podbay:latest .

clean:
	rm -rf $(BIN)
