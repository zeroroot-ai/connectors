# Each server is one directory with one package main. The repo is one Go
# module, and internal/kit holds the shared code.
SERVERS := $(patsubst %/Dockerfile,%,$(wildcard */Dockerfile))
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.Version=$(VERSION)

.PHONY: build test check check-fmt vet lint new-server check-images image

## build: compile each server into bin/.
build:
	@for s in $(SERVERS); do CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$$s ./$$s || exit 1; done

## test: run all unit tests.
test:
	go test ./...

## check: the guards that CI runs on a pull request. golangci-lint is not one
## of them. CI scopes it to new code with --new-from-rev.
check: check-fmt vet check-images

## check-fmt: fail when a file is not gofmt-clean.
check-fmt:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

## vet: report suspicious constructs.
vet:
	go vet ./...

## lint: golangci-lint on the whole module. On a workstation, run it only
## through scripts/heavy with a memory cap (see CLAUDE.md of the workspace).
lint:
	golangci-lint run ./...

## new-server: start a new server from _template. Usage: make new-server NAME=<tool>
new-server:
	@test -n "$(NAME)" || { echo "usage: make new-server NAME=<tool>"; exit 2; }
	scripts/new-server.sh "$(NAME)"

## check-images: fail when a server directory has no Dockerfile or no README.
check-images:
	scripts/check-servers_test.sh
	scripts/digest-table_test.sh
	scripts/check-servers.sh .

## image: build the image of one server. Usage: make image NAME=<tool>
image:
	@test -n "$(NAME)" || { echo "usage: make image NAME=<tool>"; exit 2; }
	docker build -f $(NAME)/Dockerfile --build-arg VERSION=$(VERSION) -t zeroroot-connectors-$(NAME):$(VERSION) .
