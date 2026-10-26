SHELL := /usr/bin/env bash
.PHONY: test build package clean

PLUGIN_NAME := nd-simple-rpc
WASM_FILE := plugin.wasm
TINYGO := $(shell command -v tinygo 2> /dev/null)

test:
	go test -race ./...

build:
ifdef TINYGO
	tinygo build -opt=2 -scheduler=none -no-debug -o $(WASM_FILE) -target wasip1 -buildmode=c-shared .
else
	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o $(WASM_FILE) .
endif

# VERSION stamps the packaged manifest (a leading "v" is dropped); unset or
# "dev" keeps the version in the tracked manifest.json, which is never modified.
VERSION ?=
MANIFEST_VERSION := $(patsubst v%,%,$(filter-out dev,$(VERSION)))

package: build
	rm -rf .pkg $(PLUGIN_NAME).ndp
	mkdir .pkg
ifeq ($(strip $(MANIFEST_VERSION)),)
	cp manifest.json .pkg/manifest.json
else
	sed -E 's|^(  "version": ")[^"]*"|\1$(MANIFEST_VERSION)"|' manifest.json > .pkg/manifest.json
	grep -qF '"version": "$(MANIFEST_VERSION)"' .pkg/manifest.json
endif
	zip -j $(PLUGIN_NAME).ndp $(WASM_FILE) .pkg/manifest.json
	rm -rf .pkg

clean:
	rm -rf $(WASM_FILE) $(PLUGIN_NAME).ndp .pkg
