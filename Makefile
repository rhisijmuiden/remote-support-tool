VERSION ?= v$(shell cat VERSION)-dev
CHANNEL ?= test
COMMIT  := $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PKG     := github.com/rhisijmuiden/remote-support-tool/server/internal/version
LDFLAGS := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).BuildDate=$(DATE) -X $(PKG).Channel=$(CHANNEL)

.PHONY: all web server test dist clean quicksupport

all: web server

web:
	cd web && npm ci && npm run build
	touch server/internal/web/dist/.gitkeep

server:
	cd server && CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o ../bin/rhis-remote ./cmd/rhis-remote

test:
	cd server && go vet ./... && go test ./...
	cd web && npx tsc -b && npx vitest run

# Release archive: the binary plus the Debian install files.
dist: all
	mkdir -p dist/pkg
	cp bin/rhis-remote deploy/debian/install.sh deploy/debian/rhis-remote.service deploy/debian/config.example.yaml dist/pkg/
	# Windows clients built by CI (release.yml puts them in dist/agent).
	if [ -d dist/agent ]; then mkdir -p dist/pkg/agent && cp dist/agent/* dist/pkg/agent/; fi
	tar -C dist/pkg -czf dist/rhis-remote_linux_amd64.tar.gz .
	cd dist && sha256sum rhis-remote_linux_amd64.tar.gz > checksums.txt

# QuickSupport for Windows; on Linux this builds a development copy (try it with --demo).
quicksupport:
	cd client && cargo build --release -p rhis-quicksupport

clean:
	rm -rf bin dist
