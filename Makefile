APP_NAME := lou-guitar
BUILD_DIR := ./bin

.PHONY: all build run clean test fmt lint tidy

all: build

## build: compile the binary into ./bin/
build:
	@mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/$(APP_NAME) .

## run: build and run the application
run: build
	$(BUILD_DIR)/$(APP_NAME)

## test: run all tests
test:
	go test ./...

## fmt: format all Go source files
fmt:
	go fmt ./...

## lint: run golangci-lint (install: https://golangci-lint.run/usage/install/)
lint:
	golangci-lint run ./...

## tidy: tidy and verify module dependencies
tidy:
	go mod tidy
	go mod verify

## clean: remove build artifacts
clean:
	rm -rf $(BUILD_DIR)

## help: print this help message
help:
	@echo "Usage:"
	@sed -n 's/^##//p' $(MAKEFILE_LIST) | column -t -s ':' | sed -e 's/^/ /'
