BINARY  := bin/siteinfo
PKG     := ./cmd/siteinfo
# golangci-lint v2 is required (v1.x cannot read Go 1.27 export data):
#   go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
LINT    ?= golangci-lint

.PHONY: build test race vet lint run tidy docker check clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BINARY) $(PKG)

test:
	go test ./...

race:
	go test -race -count=1 ./...

vet:
	go vet ./...

lint:
	$(LINT) run ./...

# Local run: single-node Redis from docker-compose, no auth (see config.example.yaml).
run:
	go run $(PKG) serve --config config.example.yaml

tidy:
	go mod tidy

docker:
	docker build -t siteinfo:dev .

# Everything CI should run.
check: build vet race lint

clean:
	rm -rf bin coverage.out
