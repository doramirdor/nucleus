BINARY  := nucleus
PKG     := ./cmd/nucleus
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build install run tidy test test-race vet fmt clean release-snapshot demo \
        launch-check launch-tag launch-dry

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

# test-race is the gate run on launch-check — race-clean tests are
# what proves sticky/audit/reaper concurrency is actually safe.
test-race:
	go test ./... -race

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

# launch-check runs every gate that has to pass before tagging a
# release. Fails fast: build, vet, race-clean tests, doctor pass.
# Run this on T-1 day per docs/launch/checklist.md before tagging.
launch-check:
	@echo "==> build"
	@$(MAKE) -s build
	@echo "==> go vet"
	@$(MAKE) -s vet
	@echo "==> go test -race"
	@$(MAKE) -s test-race
	@echo "==> nucleus doctor"
	@./bin/$(BINARY) doctor || (echo "doctor reported failures — fix before launch"; exit 1)
	@echo
	@echo "Launch check passed."

# launch-tag prints the exact tag command. Doesn't run it — tagging
# the release is intentionally a manual step so an over-eager 'make'
# can't accidentally cut a release.
launch-tag:
	@echo "Cut the release with:"
	@echo
	@echo "  git tag v0.2.0"
	@echo "  git push origin v0.2.0"
	@echo
	@echo "GoReleaser CI takes it from there. See docs/launch/checklist.md."

# launch-dry runs everything launch-check does PLUS a dry-run of the
# release pipeline (snapshot, no publish). Useful for confirming the
# Homebrew tap / archives are well-formed before tagging.
launch-dry:
	@$(MAKE) -s launch-check
	@echo "==> goreleaser snapshot"
	@$(MAKE) -s release-snapshot
