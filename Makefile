GOBIN_DIR := $(shell go env GOPATH)/bin
export PATH := $(GOBIN_DIR):$(PATH)

MODULE    := github.com/example/scada-gas
PROTO_SRC := proto/v1/scada_gas.proto
BIN_DIR   := bin

.PHONY: all tools proto build run-sim run-core run-fep test clean

all: proto build

# Install the protoc Go plugins (protoc itself: apt install protobuf-compiler).
tools:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

proto:
	protoc -I proto \
		--go_out=gen --go_opt=module=$(MODULE)/gen \
		--go-grpc_out=gen --go-grpc_opt=module=$(MODULE)/gen \
		$(PROTO_SRC)

build: proto
	mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/simulator ./cmd/simulator
	go build -o $(BIN_DIR)/fep ./cmd/fep
	go build -o $(BIN_DIR)/core ./cmd/core

run-sim:
	go run ./cmd/simulator

run-core:
	go run ./cmd/core

run-fep:
	go run ./cmd/fep

test:
	go test -race ./...

clean:
	rm -rf $(BIN_DIR)
