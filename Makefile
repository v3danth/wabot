# Commands for wabot. Run them from this folder, e.g. `make api`.
# Both programs share session.db here, so always run from the repo root.

.PHONY: help api bot test lint build clean

help: ## List the commands
	@grep -E '^[a-z]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  make %-6s %s\n", $$1, $$2}'

api: ## Start the HTTP API on 127.0.0.1:8080 (shows a QR code on first run)
	go run ./cmd/api

bot: ## Start the interactive bot (terminal + WhatsApp commands)
	go run ./cmd/bot

test: ## Run all tests (no WhatsApp login needed)
	go test ./...

lint: ## Format check, vet and lint
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "run: gofmt -w ." && exit 1)
	go vet ./...
	golangci-lint run ./...

build: ## Build both programs into ./bin
	go build -o bin/api ./cmd/api
	go build -o bin/bot ./cmd/bot

clean: ## Remove built programs (keeps your login and chat logs)
	rm -rf bin
