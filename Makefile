VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test vet docker docker-prebuilt release
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/companion ./cmd/companion
test:
	go test -race -count=1 ./...
vet:
	gofmt -l . && go vet ./...
docker:
	docker compose build
release:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/companion-linux-amd64 ./cmd/companion
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/companion-linux-arm64 ./cmd/companion
docker-prebuilt: release
	docker build -f Dockerfile.prebuilt -t companion:latest .
