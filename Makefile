PLUGIN_ID  := web-search-redirect
GOOS       := $(shell go env GOOS)
GOARCH     := $(shell go env GOARCH)
EXT        := $(if $(filter darwin,$(GOOS)),dylib,$(if $(filter windows,$(GOOS)),dll,so))
OUT_DIR    := bin/$(GOOS)/$(GOARCH)
ARTIFACT   := $(OUT_DIR)/$(PLUGIN_ID).$(EXT)
# Directory configured as plugins.dir in the CLIProxyAPI config.
PLUGINS_DIR ?= $(HOME)/.cli-proxy-api/plugins

.PHONY: build test install clean

build:
	@mkdir -p $(OUT_DIR)
	CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o $(ARTIFACT) .
	@rm -f $(OUT_DIR)/$(PLUGIN_ID).h
	@echo "built $(ARTIFACT)"

test:
	go test ./...

install: build
	@mkdir -p $(PLUGINS_DIR)/$(GOOS)/$(GOARCH)
	cp $(ARTIFACT) $(PLUGINS_DIR)/$(GOOS)/$(GOARCH)/
	@echo "installed to $(PLUGINS_DIR)/$(GOOS)/$(GOARCH)/ (restart cliproxyapi to load)"

clean:
	rm -rf bin
