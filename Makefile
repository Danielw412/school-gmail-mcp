# Build, test and install the School Email Sync backend.
#   make build     compile backend/bin/school-email-sync
#   make test      Go tests + Apps Script scenario tests (needs node >= 18)
#   make install   build, install to ~/.local/bin and (re)start the user service

export GOTOOLCHAIN := local
GO      ?= go
BIN     := backend/bin/school-email-sync
VERSION := $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)$(shell git diff --quiet HEAD 2>/dev/null || echo -dirty)
TAGS    := sqlite_fts5

.PHONY: build test test-go test-apps-script install clean

build:
	cd backend && CGO_ENABLED=1 nice $(GO) build -tags $(TAGS) -trimpath \
		-ldflags "-s -w -X main.version=$(VERSION)" -o bin/school-email-sync .

test: test-go test-apps-script

test-go:
	cd backend && nice $(GO) test -tags $(TAGS) -count=1 ./...

test-apps-script: build
	node --test tests/apps-script/code-gs.test.js

install: build
	deploy/install-backend.sh

clean:
	rm -rf backend/bin
