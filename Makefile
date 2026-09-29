SHELL := /bin/bash
BIN := bin/monstermq-edge
PKG := ./cmd/monstermq-edge

VERSION := $(shell cat version.txt 2>/dev/null | tr -d '\n' | tr -d '\r')
LDFLAGS := -s -w -X monstermq.io/edge/internal/version.Version=$(VERSION)
GOFLAGS := -trimpath

.PHONY: build build-arm64 build-armv7 test test-race lint clean gen run deb-arm64 deb-armv7 deb-amd64 deb-all release publish prepare-dashboard embed-lib embed-test embed-check

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
	go test ./... -count=1 -timeout 300s

test-race:
	go test ./... -race -count=1 -timeout 600s

lint:
	go vet ./...

# Opt-in WinCC OA embedding library (the only CGO target, see
# dev/plans/spec-winccoa-native.md). Standalone targets stay CGO_ENABLED=0.
EMBED_OUT := build/embed

embed-lib:
	mkdir -p $(EMBED_OUT)
	CGO_ENABLED=1 go build $(GOFLAGS) -tags winccoa_embed -buildmode=c-archive -o $(EMBED_OUT)/libmonstermq.a ./embed/cabi
	cp embed/cabi/monstermq.h embed/cabi/monstermq_types.h $(EMBED_OUT)/

# Links the library into a plain C host and exercises the ABI contract.
embed-test: embed-lib
	$(CC) -std=c11 -Wall -Wextra -Werror -I$(EMBED_OUT) -o $(EMBED_OUT)/abi_harness embed/harness/abi_harness.c $(EMBED_OUT)/libmonstermq.a -lpthread
	cd embed/harness && ../../$(EMBED_OUT)/abi_harness

# Memory and pointer diagnostics for the embedding library (AC-09): the C
# harness against a cgocheck2 build and against an AddressSanitizer build.
embed-check:
	mkdir -p build/embed-check build/embed-asan
	GOEXPERIMENT=cgocheck2 CGO_ENABLED=1 go build $(GOFLAGS) -tags winccoa_embed -buildmode=c-archive -o build/embed-check/libmonstermq.a ./embed/cabi
	cp embed/cabi/monstermq.h embed/cabi/monstermq_types.h build/embed-check/
	$(CC) -std=c11 -Wall -Wextra -Werror -Ibuild/embed-check -o build/embed-check/abi_harness embed/harness/abi_harness.c build/embed-check/libmonstermq.a -lpthread
	cd embed/harness && ../../build/embed-check/abi_harness
	CGO_ENABLED=1 go build -asan $(GOFLAGS) -tags winccoa_embed -buildmode=c-archive -o build/embed-asan/libmonstermq.a ./embed/cabi
	cp embed/cabi/monstermq.h embed/cabi/monstermq_types.h build/embed-asan/
	$(CC) -std=c11 -g -fsanitize=address -fno-omit-frame-pointer -Wall -Wextra -Werror -Ibuild/embed-asan -o build/embed-asan/abi_harness embed/harness/abi_harness.c build/embed-asan/libmonstermq.a -lpthread
	cd embed/harness && ASAN_OPTIONS=detect_leaks=1 ../../build/embed-asan/abi_harness

clean:
	rm -rf bin dist build/embed build/embed-check build/embed-asan
	@find internal/dashboard/dist -mindepth 1 ! -name 'placeholder.html' -exec rm -rf {} + 2>/dev/null || true

gen:
	go run github.com/99designs/gqlgen generate

run: build
	$(BIN) -config config.yaml.example

