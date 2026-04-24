BINARY  := nucleus
PKG     := ./cmd/nucleus
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build install run tidy test vet fmt clean release-snapshot demo

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(PKG)

install:
	CGO_ENABLED=0 go install -ldflags "$(LDFLAGS)" $(PKG)

run: build
	./bin/$(BINARY) serve

tidy:
	go mod tidy

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

clean:
	rm -rf bin/ dist/

release-snapshot:
	goreleaser release --snapshot --clean

demo:
	@command -v vhs >/dev/null 2>&1 || { echo "vhs not installed — see demo/README.md"; exit 1; }
	vhs demo/overview.tape
	vhs demo/multi-profile.tape
