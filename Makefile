APP_NAME  := lou-guitar
BUILD_DIR := ./bin
GO_DIR    := ./go
ARD_DIR   := ./arduino/guitar_controller
ARD_FQBN  := arduino:avr:leonardo
ARD_PORT  := /dev/ttyACM0

.PHONY: all build run clean test fmt lint tidy arduino arduino-build arduino-upload

all: build

## build: compile the binary into ./bin/
build:
	@mkdir -p $(BUILD_DIR)
	cd $(GO_DIR) && go build -o ../$(BUILD_DIR)/$(APP_NAME) .

## run: build and run the application
run: build
	$(BUILD_DIR)/$(APP_NAME)

## test: run all tests
test:
	cd $(GO_DIR) && go test ./...

## fmt: format all Go source files
fmt:
	cd $(GO_DIR) && go fmt ./...

## lint: run golangci-lint (install: https://golangci-lint.run/usage/install/)
lint:
	cd $(GO_DIR) && golangci-lint run ./...

## tidy: tidy and verify module dependencies
tidy:
	cd $(GO_DIR) && go mod tidy && go mod verify

## arduino: compile and upload the Arduino sketch
arduino: arduino-build arduino-upload

## arduino-build: compile the Arduino sketch (no upload)
arduino-build:
	arduino-cli compile --fqbn $(ARD_FQBN) $(ARD_DIR)

## arduino-upload: upload a previously compiled sketch (board must be connected)
arduino-upload:
	arduino-cli upload --fqbn $(ARD_FQBN) --port $(ARD_PORT) $(ARD_DIR)

## clean: remove build artifacts
clean:
	rm -rf $(BUILD_DIR)

## help: print this help message
help:
	@echo "Usage:"
	@sed -n 's/^##//p' $(MAKEFILE_LIST) | column -t -s ':' | sed -e 's/^/ /'
