SHELL := /bin/bash

.PHONY: dev build build-go build-cpp test test-go test-cpp lint lint-go lint-cpp chaos chaos-sigterm fuzz coverage-gate bench bench-smoke bench-regress up clean

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

# Quick fuzz pass on the WAL/checkpoint parser. CI invokes this with a
# short fuzztime; locally bump FUZZTIME to spend more time per target.
FUZZTIME ?= 15s
fuzz:
	go test -run='^$$' -fuzz=FuzzParse -fuzztime=$(FUZZTIME) ./internal/checkpoint
	go test -run='^$$' -fuzz=FuzzEncodeParseRoundtrip -fuzztime=$(FUZZTIME) ./internal/checkpoint
	go test -run='^$$' -fuzz=FuzzBitFlipDetected -fuzztime=$(FUZZTIME) ./internal/checkpoint
	go test -run='^$$' -fuzz=FuzzRandomBytesNoPanic -fuzztime=$(FUZZTIME) ./internal/checkpoint

# Coverage gate: enforces minimum total coverage. Floor lifts +5pp over the
# layer-0 baseline (32.9%) to 37.9%.
COVERAGE_FLOOR ?= 37.9
coverage-gate:
	@CGO_ENABLED=1 go test -count=1 -coverprofile=coverage.out ./internal/... >/dev/null
	@total=$$(go tool cover -func=coverage.out | awk '/^total:/ {sub(/%$$/, "", $$3); print $$3}'); \
	  awk -v t="$$total" -v f="$(COVERAGE_FLOOR)" 'BEGIN { if (t+0 < f+0) { printf("coverage %s%% below floor %s%%\n", t, f); exit 1 } else { printf("coverage %s%% >= floor %s%%\n", t, f) } }'

# 1000-job concurrent bench. Spawns N=1000 worker subprocesses, measures
# submit-to-complete latency P50/P95/P99 and throughput. Output JSON lands
# in bench/results/<timestamp>.json plus bench/results/latest.json.
BENCH_N ?= 1000
BENCH_CONC ?= 64
BENCH_LIMIT ?= 10000
BENCH_EVERY ?= 1000
bench: build-cpp
	@mkdir -p bin bench/results
	go build -o bin/concurrent_bench ./bench/concurrent
	./bin/concurrent_bench -n $(BENCH_N) -concurrency $(BENCH_CONC) -limit $(BENCH_LIMIT) \
	  -checkpoint-every $(BENCH_EVERY) -worker worker/build/jobworker -out-dir bench/results

# CI smoke at N=20 -- proves the bench harness works without paying the
# price of a full 1000-job run on every push.
bench-smoke: build-cpp
	@mkdir -p bin bench/results
	go build -o bin/concurrent_bench ./bench/concurrent
	./bin/concurrent_bench -n 20 -concurrency 8 -limit $(BENCH_LIMIT) \
	  -checkpoint-every $(BENCH_EVERY) -worker worker/build/jobworker -out-dir bench/results

# bench-regress gate: compare bench/results/latest.json to bench/baseline.json
# and exit non-zero on regression.
bench-regress:
	@mkdir -p bin
	go build -o bin/bench_regress ./bench/regress
	./bin/bench_regress

chaos:
	bench/chaos.sh

chaos-sigterm:
	bench/chaos-sigterm.sh

up:
	docker compose up --build

clean:
	rm -rf bin worker/build bench/runtime
