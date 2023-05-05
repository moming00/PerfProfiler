VERSION=$(shell git describe --tags --dirty)
LDFLAGS += -X "main.version=$(VERSION)"
LDFLAGS += -X "main.date=$(shell date --iso-8601=s)"
LDFLAGS += -X "main.commit=$(shell git rev-parse HEAD)"
LDFLAGS += -X "main.builtBy=$(shell echo `whoami`@`hostname`)"
export GOPROXY=https://goproxy.cn,direct
CONTAINER_COMMAND_CFLAGS := -O2 -g -Wall -Werror $(CFLAGS)
CONTAINER_COMMAND_CLANG ?= clang
REPODIR := $(shell dirname $(realpath $(firstword $(MAKEFILE_LIST))))/

GO        := go
GOBUILD   := $(GO) build $(BUILD_FLAG)


.PHONY: pre
pre:
	go mod tidy

.PHONY: build
build: pre
	go generate .
	$(GOBUILD) -ldflags '$(LDFLAGS)' -o . ./...

.PHONY: debug
debug: pre
	$(GOBUILD) -ldflags '$(LDFLAGS)' -gcflags "all=-N -l" -o . ./...

.PHONY: benchtest
benchtest: pre
	go test -bench=. ./...

.PHONY: systest
systest: build
	bash go.test.sh
	bash go.metrictest.sh

.PHONY: gotest
gotest: pre
	go test -v ./... -coverprofile=coverage.out -covermode count
	go tool cover -func coverage.out

.PHONY: lint
lint:
	golangci-lint run -D errcheck,govet,gosimple

.PHONY: run
run: pre
	go run cmd/clickhouse_sinker/main.go --local-cfg-file docker/test_dynamic_schema.hjson

.PHONY: generate
generate: export BPF_CLANG := $(CONTAINER_COMMAND_CLANG)
generate: export BPF_CFLAGS := $(CONTAINER_COMMAND_CFLAGS)
generate: export REPO_ROOT := $(REPODIR)
generate:
	cd ./ && TARGET=$(if $(findstring x86_64,$(shell uname -m)),amd64,arm64) go generate ./...