BINARY  := bin/siteinfo
PKG     := ./cmd/siteinfo
# Tools (golangci-lint v2 is required; v1.x cannot read Go 1.27 export data):
#   go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
#   go install golang.org/x/vuln/cmd/govulncheck@latest
LINT    ?= golangci-lint
VULN    ?= govulncheck

.PHONY: build test race vet lint vuln run up down update tidy docker check clean

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

# Known vulnerabilities in dependencies and the Go standard library that the code can reach.
vuln:
	$(VULN) ./...

# Local run against Redis from docker-compose, auth off (see config.example.yaml).
run:
	go run $(PKG) serve --config config.example.yaml

# Full local stack: Redis, the service (debug mode) and the debug UI on http://localhost:8081.
up:
	docker compose up --build -d

down:
	docker compose down

# Update all Go dependencies to their newest versions.
update:
	go get -u -t ./...
	go mod tidy

tidy:
	go mod tidy

docker:
	docker build -t siteinfo:dev .

# Everything CI should run.
check: build vet race lint vuln

clean:
	rm -rf bin coverage.out
