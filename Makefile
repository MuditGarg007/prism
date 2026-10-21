VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: all build test vet staticcheck lint bench fuzz validate chaos reload clean

all: lint test

build:
	go build -ldflags "$(LDFLAGS)" -o bin/prism ./cmd/prism

test:
	go test -race ./...

vet:
	go vet ./...

staticcheck:
	go tool staticcheck ./...

lint: vet staticcheck
	@test -z "$$(gofmt -l .)" || (echo "needs gofmt:"; gofmt -l .; exit 1)

bench:
	go test -run='^$$' -bench=. -benchmem ./...

fuzz:
	go test -run='^$$' -fuzz=FuzzParse -fuzztime=30s ./internal/config
	go test -run='^$$' -fuzz=FuzzMatch -fuzztime=30s ./internal/router
	go test -run='^$$' -fuzz=FuzzClientIP -fuzztime=30s ./internal/proxy
	go test -run='^$$' -fuzz=FuzzValidPath -fuzztime=30s ./internal/proxy
	go test -run='^$$' -fuzz=FuzzTraceparent -fuzztime=30s ./internal/proxy
	go test -run='^$$' -fuzz=FuzzGCRA -fuzztime=30s ./internal/ratelimit
	go test -run='^$$' -fuzz=FuzzWindow -fuzztime=30s ./internal/resilience
	go test -run='^$$' -fuzz=FuzzBudget -fuzztime=30s ./internal/resilience
	go test -run='^$$' -fuzz=FuzzCacheControl -fuzztime=30s ./internal/cache

validate: build
	./bin/prism -config examples/prism.yaml -validate

# About 90 s. Results and a graph.svg for each scenario go to bin/chaos.
chaos:
	scripts/chaos/kill-backend.sh
	scripts/chaos/outage.sh

# About 25 s: SIGHUP every 0.25 s under load, failing on any error.
reload:
	scripts/reload-under-load.sh

clean:
	rm -rf bin
