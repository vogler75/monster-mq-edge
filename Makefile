SHELL := /bin/bash
BIN := bin/monstermq-edge
PKG := ./cmd/monstermq-edge

VERSION := $(shell cat version.txt 2>/dev/null | tr -d '\n' | tr -d '\r')
LDFLAGS := -s -w -X monstermq.io/edge/internal/version.Version=$(VERSION)
GOFLAGS := -trimpath

.PHONY: build build-arm64 build-armv7 test test-race lint clean gen run deb-arm64 deb-armv7 deb-amd64 deb-all release publish prepare-dashboard

prepare-dashboard:
	@if [ -e "dashboard" ] || [ -L "dashboard" ]; then \
		if [ ! -d "dashboard" ]; then \
			rm -f dashboard; \
		fi; \
	fi; \
	if [ ! -e "dashboard" ] && [ ! -L "dashboard" ]; then \
		if [ -d "../dashboard" ]; then \
			echo "Symlink 'dashboard' not found. Creating symlink to ../dashboard..."; \
			ln -sfn ../dashboard dashboard; \
		else \
			echo "Error: Dashboard not found! Neither ./dashboard nor ../dashboard exists." >&2; \
			exit 1; \
		fi; \
	fi; \
	if [ ! -d "dashboard/dist" ]; then \
		echo "Building dashboard..."; \
		(cd dashboard && npm run build); \
	fi; \
	echo "Syncing dashboard from dashboard/dist..."; \
	mkdir -p internal/dashboard/dist && cp -r dashboard/dist/* internal/dashboard/dist/

build: prepare-dashboard
	@mkdir -p bin
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags="$(LDFLAGS)" -o $(BIN) $(PKG)

build-arm64: prepare-dashboard
	@mkdir -p bin
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build $(GOFLAGS) -ldflags="$(LDFLAGS)" -o bin/monstermq-edge-linux-arm64 $(PKG)

build-armv7: prepare-dashboard
	@mkdir -p bin
	GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build $(GOFLAGS) -ldflags="$(LDFLAGS)" -o bin/monstermq-edge-linux-armv7 $(PKG)

build-amd64: prepare-dashboard
	@mkdir -p bin
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build $(GOFLAGS) -ldflags="$(LDFLAGS)" -o bin/monstermq-edge-linux-amd64 $(PKG)

build-all: build-amd64 build-arm64 build-armv7

deb-arm64:
	./scripts/build-deb.sh --arch arm64

deb-armv7:
	./scripts/build-deb.sh --arch armhf

deb-amd64:
	./scripts/build-deb.sh --arch amd64

deb-all: deb-arm64 deb-armv7 deb-amd64

release:
	./release.sh

publish:
	./publish.sh

test:
	go test ./... -count=1 -timeout 60s

test-race:
	go test ./... -race -count=1 -timeout 120s

lint:
	go vet ./...

clean:
	rm -rf bin dist
	@find internal/dashboard/dist -mindepth 1 ! -name 'placeholder.html' -exec rm -rf {} + 2>/dev/null || true

gen:
	go run github.com/99designs/gqlgen generate

run: build
	$(BIN) -config config.yaml.example

