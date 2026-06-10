.PHONY: test test-hermes test-forge test-e2e build clean help

help:
	@printf 'Targets:\n'
	@printf '  make test        — run all Go tests\n'
	@printf '  make build       — build Hermes and Forge binaries into ./bin\n'
	@printf '  make clean       — remove local build outputs\n'

test: test-hermes test-forge test-e2e

test-hermes:
	cd hermes && go test ./...

test-forge:
	cd forge && go test ./...

test-e2e:
	cd test/e2e && go test ./...

build:
	mkdir -p bin
	cd hermes && go build -o ../bin/hermes ./cmd/hermes
	cd hermes && go build -o ../bin/hermes-mcp ./cmd/hermes-mcp
	cd forge && go build -o ../bin/forge ./cmd/forge

clean:
	rm -rf bin
