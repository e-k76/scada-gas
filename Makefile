GOBIN_DIR := $(shell go env GOPATH)/bin
export PATH := $(GOBIN_DIR):$(PATH)

MODULE    := github.com/example/scada-gas
PROTO_SRC := proto/v1/scada_gas.proto
BIN_DIR   := bin

.PHONY: all tools proto build run-sim run-core run-fep run-historian run-historian-log run-gateway alarms ack-all alarm-shell alarm-watch db-up db-down db-reset db-psql test clean

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
	go build -o $(BIN_DIR)/historian ./cmd/historian
	go build -o $(BIN_DIR)/alarmcli ./cmd/alarmcli
	go build -o $(BIN_DIR)/gateway ./cmd/gateway

run-sim:
	go run ./cmd/simulator

run-core:
	go run ./cmd/core

run-fep:
	go run ./cmd/fep

# Historian writing to TimescaleDB (start it first with `make db-up`).
run-historian:
	go run ./cmd/historian

# Historian without a database: prints the rows it would store.
run-historian-log:
	go run ./cmd/historian -log-only

# Northbound API + WebSocket + dashboard on http://localhost:8080
run-gateway:
	go run ./cmd/gateway

# Operator console for the alarm API.
alarms:
	go run ./cmd/alarmcli list

ack-all:
	go run ./cmd/alarmcli ack-all

alarm-shell:
	go run ./cmd/alarmcli shell

alarm-watch:
	go run ./cmd/alarmcli watch

# TimescaleDB container (waits until the healthcheck passes).
db-up:
	docker compose up -d --wait timescaledb

db-down:
	docker compose down

db-reset:
	docker compose down -v

db-psql:
	docker compose exec timescaledb psql -U scada -d scada

test:
	go test -race ./...

clean:
	rm -rf $(BIN_DIR)
