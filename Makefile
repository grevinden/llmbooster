.PHONY: build clean test

BUILD_DIR := .
PLUGIN_NAME := llmboster.so

build:
	go build -buildmode=plugin -o $(BUILD_DIR)/$(PLUGIN_NAME) -ldflags "-X main.buildCommit=$(shell git rev-parse --short HEAD 2>/dev/null || echo dev) -X main.buildTime=$(shell date +%s)" ./src/

clean:
	rm -f $(BUILD_DIR)/$(PLUGIN_NAME)

test:
	go test -v -count=1 ./src/
