# Developer commands for the Notploy GitHub Action.
# `make check` mirrors what the CI runs.

BINARY  := bin/notploy-action
VERSION ?= dev
IMAGE   ?= notploy-action

.PHONY: all build test fmt fmt-check vet lint actionlint validate check image clean

all: check

build:
	go build -trimpath -ldflags "-s -w -X github.com/skygenesisenterprise/notploy-actions/internal/version.Version=$(VERSION)" -o $(BINARY) ./cmd/notploy-action

test:
	go test ./... -count=1

fmt:
	gofmt -w cmd internal

fmt-check:
	@unformatted="$$(gofmt -l cmd internal)"; \
	if [ -n "$$unformatted" ]; then \
		echo "not gofmt-formatted:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

vet:
	go vet ./...

lint:
	go run github.com/golangci/golangci-lint/cmd/golangci-lint@v1.64.8 run ./...

actionlint:
	go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.7

# The npm package of that name is a different project; the CLI is
# `@action-validator/cli`, whose binary is `action-validator`.
validate:
	npx -y @action-validator/cli@0.6.0 action.yml

check: fmt-check vet test lint validate actionlint

image:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

clean:
	rm -rf bin
