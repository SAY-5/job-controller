SHELL := /bin/bash

.PHONY: dev build build-go build-cpp test test-go test-cpp lint lint-go lint-cpp chaos chaos-sigterm up clean

# Local dev build (Go binaries + C++ worker)
dev: build

build: build-go build-cpp

build-go:
	@mkdir -p bin
	CGO_ENABLED=1 go build -o bin/controller ./cmd/controller
	CGO_ENABLED=1 go build -o bin/reaper ./cmd/reaper

build-cpp:
	cmake -B worker/build -S worker -DCMAKE_BUILD_TYPE=Release
	cmake --build worker/build -j

test: test-go test-cpp

test-go:
	CGO_ENABLED=1 go test -race ./...

test-cpp:
	cd worker/build && ctest --output-on-failure

lint: lint-go lint-cpp

lint-go:
	@if command -v golangci-lint >/dev/null 2>&1; then \
	  golangci-lint run ./... ; \
	else \
	  echo "golangci-lint not installed; running go vet" ; \
	  go vet ./... ; \
	fi

lint-cpp:
	@if command -v clang-format >/dev/null 2>&1; then \
	  find worker/src worker/tests -type f \( -name '*.cpp' -o -name '*.h' \) \
	    | xargs clang-format --dry-run -Werror ; \
	else \
	  echo "clang-format not installed; skipping (CI runs the real check)" ; \
	fi

chaos:
	bench/chaos.sh

chaos-sigterm:
	bench/chaos-sigterm.sh

up:
	docker compose up --build

clean:
	rm -rf bin worker/build bench/runtime
