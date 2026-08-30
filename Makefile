GO ?= go
BIN_DIR ?= $(CURDIR)/bin
DIST_DIR ?= $(CURDIR)/dist
BIN_NAME ?= ban-dislike-bot
GOARCH ?= amd64
GOLANGCI_LINT_VERSION ?= v2.12.0
LINT_BIN := $(BIN_DIR)/golangci-lint
LINT_CACHE ?= $(BIN_DIR)/.golangci-cache

.PHONY: build build-linux test test-race vet fmt fmt-check lint verify clean install-lint

build:
	mkdir -p $(BIN_DIR)
	$(GO) build -trimpath -o $(BIN_DIR)/$(BIN_NAME) ./cmd/ban-dislike-bot

build-linux:
	mkdir -p $(DIST_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) $(GO) build -trimpath -ldflags="-s -w" -o $(DIST_DIR)/$(BIN_NAME) ./cmd/ban-dislike-bot

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

fmt-check:
	@files="$$(gofmt -l .)"; \
	if [ -n "$$files" ]; then \
		echo "These files need gofmt:"; \
		echo "$$files"; \
		exit 1; \
	fi

install-lint: $(LINT_BIN)

$(LINT_BIN):
	mkdir -p $(BIN_DIR)
	GOBIN=$(BIN_DIR) $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

lint: $(LINT_BIN)
	mkdir -p $(LINT_CACHE)
	GOLANGCI_LINT_CACHE=$(LINT_CACHE) $(LINT_BIN) run ./...

verify: fmt-check vet test build

clean:
	rm -f $(BIN_DIR)/$(BIN_NAME) $(DIST_DIR)/$(BIN_NAME)
