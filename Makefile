# igradle — interactive Gradle task launcher
#
# Single static binary, cross-compiled for linux/darwin/windows on amd64+arm64.

BIN       := igradle
PKG       := .
LDFLAGS   := -s -w
BUILD_DIR := bin

# Static, no CGO so the binary runs anywhere without libc shims.
export CGO_ENABLED := 0

.PHONY: all build build-all install clean tidy run help

all: build ## Build for the current platform

build: ## Build for the host OS/arch
	@mkdir -p $(BUILD_DIR)
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BUILD_DIR)/$(BIN) $(PKG)
	@echo "Built $(BUILD_DIR)/$(BIN)"

# Each entry: GOOS/GOARCH → output name
PLATFORMS := \
	linux/amd64 \
	linux/arm64 \
	darwin/amd64 \
	darwin/arm64 \
	windows/amd64 \
	windows/arm64

build-all: ## Cross-compile for all supported platforms
	@mkdir -p $(BUILD_DIR)
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		ext=$$( [ "$$os" = "windows" ] && echo .exe || echo ); \
		echo "→ $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch \
			go build -trimpath -ldflags '$(LDFLAGS)' \
				-o $(BUILD_DIR)/$(BIN)-$$os-$$arch$$ext $(PKG); \
	done
	@echo ""
	@ls -lh $(BUILD_DIR)/

install: build ## Install to $$GOBIN (or ~/go/bin)
	@install -m 0755 $(BUILD_DIR)/$(BIN) "$${GOBIN:-$$HOME/go/bin}/$(BIN)"

tidy: ## Sync go.mod / go.sum
	go mod tidy

run: build ## Build then run with any extra args
	@$(BUILD_DIR)/$(BIN) $(ARGS)

clean: ## Remove built binaries
	rm -rf $(BUILD_DIR)

help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "Targets:\n"} \
		/^[a-zA-Z_-]+:.*?##/ { printf "  %-12s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

.DEFAULT_GOAL := help
