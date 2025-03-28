.PHONY: all build test lint clean examples benchmark generate-fb check-flatbuffers test-core coverage-report test-coverage-focused check-coverage coverage-improvement verify-slsa codecov-coverage property-tests

# Default target
all: generate-fb lint test build

# Build the project
build: generate-fb
	go build ./...

# Run tests
test: generate-fb
	go test -race ./...

# Run property-based tests
property-tests: generate-fb
	./scripts/testing/run_property_tests.sh

# Run tests with coverage
test-coverage: generate-fb
	go test -race -coverprofile=coverage.txt -covermode=atomic ./...
	go tool cover -html=coverage.txt -o coverage.html

# Run tests with coverage for core packages only (excluding examples, benchmarks, and generated code)
test-coverage-focused: generate-fb
	go test -race -coverprofile=coverage-focused.txt -covermode=atomic `go list ./... | grep -v "examples\|fb\|benchmark"`
	go tool cover -html=coverage-focused.txt -o coverage-focused.html

# Generate coverage report specifically for codecov
codecov-coverage: generate-fb
	@echo "Generating coverage report for codecov..."
	@# Run tests for all packages
	@go test -race ./...
	@# Generate coverage report for relevant packages only
	@go test -race -coverprofile=coverage.txt -covermode=atomic `go list ./... | grep -v "examples\|fb\|benchmark"`
	@echo "Coverage report generated at coverage.txt"
	@echo "Coverage by priority level:"
	@echo "Critical (pkg/workflow): $(shell go tool cover -func=coverage.txt | grep "pkg/workflow" | grep -v "pkg/workflow/fb" | grep total | awk '{print $$3}')"
	@echo "High (arena, memory): $(shell go tool cover -func=coverage.txt | grep "internal/workflow/arena\|internal/workflow/memory" | grep total | awk '{print $$3}')"
	@echo "Medium (metrics, utils, concurrent): $(shell go tool cover -func=coverage.txt | grep "internal/workflow/metrics\|internal/workflow/utils\|internal/workflow/concurrent" | grep total | awk '{print $$3}')"

# Run tests for core functionality only
test-core: generate-fb
	go test -race -coverprofile=coverage-core.txt -covermode=atomic ./pkg/workflow ./internal/workflow/arena ./internal/workflow/memory
	go tool cover -func=coverage-core.txt
	go tool cover -html=coverage-core.txt -o coverage-core.html

# Check if coverage meets thresholds
check-coverage:
	./scripts/testing/check_coverage.sh

# Generate a focused coverage report
coverage-report: test-coverage-focused
	@echo "Coverage by package:"
	@go tool cover -func=coverage-focused.txt | grep -v "examples\|fb\|benchmark" | sort -k 3 -r
	@echo "Overall coverage of core packages:"
	@go tool cover -func=coverage-focused.txt | grep total:

# Generate a report showing which files need coverage improvement
coverage-improvement:
	@echo "Files needing coverage improvement:"
	@echo "==================================="
	@for pkg in workflow; do \
		echo "\nPackage: pkg/$$pkg"; \
		pkg_file=$$(echo $$pkg | tr '/' '-'); \
		go test -coverprofile=coverage-temp-$$pkg_file.txt -covermode=atomic ./pkg/$$pkg || true; \
		if [ -f coverage-temp-$$pkg_file.txt ]; then \
			go tool cover -func=coverage-temp-$$pkg_file.txt | grep -v "total:" | awk '{if ($$3 < "50.0%") print $$1 ": " $$3}'; \
			rm -f coverage-temp-$$pkg_file.txt; \
		else \
			echo "No coverage data generated for pkg/$$pkg (package may not exist or have no tests)"; \
		fi; \
	done
	@for pkg in workflow/arena workflow/memory workflow/metrics workflow/utils workflow/concurrent; do \
		echo "\nPackage: internal/$$pkg"; \
		pkg_file=$$(echo $$pkg | tr '/' '-'); \
		go test -coverprofile=coverage-temp-$$pkg_file.txt -covermode=atomic ./internal/$$pkg || true; \
		if [ -f coverage-temp-$$pkg_file.txt ]; then \
			go tool cover -func=coverage-temp-$$pkg_file.txt | grep -v "total:" | awk '{if ($$3 < "50.0%") print $$1 ": " $$3}'; \
			rm -f coverage-temp-$$pkg_file.txt; \
		else \
			echo "No coverage data generated for internal/$$pkg (package may not exist or have no tests)"; \
		fi; \
	done

# Run linter
lint:
	golangci-lint run --config .golangci.yml

# Clean build artifacts
clean:
	rm -f coverage*.txt coverage*.html
	rm -rf ./bin
	rm -rf pkg/workflow/fb

# FlatBuffers related targets
check-flatbuffers:
	./scripts/tools/check_flatbuffers.sh

generate-fb: check-flatbuffers
	mkdir -p pkg/workflow/fb
	flatc --go -o pkg/workflow/fb pkg/workflow/schema/workflow_data.fbs

# Run examples
examples:
	@for example in $$(find examples -name "main.go" -not -path "*/\.*" | sort); do \
		dir=$$(dirname $$example); \
		echo "Running example: $$dir"; \
		(cd $$dir && go build -v && cd -); \
	done

# Run benchmarks
benchmark:
	go test -bench=. -benchmem ./internal/workflow/benchmark/...

# Generate benchmark profiles
benchmark-profile:
	mkdir -p benchmark_profiles/cpu benchmark_profiles/mem
	go test -bench=. -benchmem -cpuprofile=benchmark_profiles/cpu/profile.out -memprofile=benchmark_profiles/mem/profile.out ./internal/workflow/benchmark/...

# View CPU profile
profile-cpu:
	go tool pprof -http=:8080 benchmark_profiles/cpu/profile.out

# View memory profile
profile-mem:
	go tool pprof -http=:8081 benchmark_profiles/mem/profile.out

# Verify SLSA provenance
verify-slsa:
	@echo "Verifying SLSA provenance..."
	@if ! command -v slsa-verifier &> /dev/null; then \
		echo "Installing SLSA verifier..."; \
		go install github.com/slsa-framework/slsa-verifier/v2/cli/slsa-verifier@latest; \
	fi
	@echo "Checking latest release for provenance..."
	@latest_tag=$$(git describe --tags --abbrev=0 2>/dev/null || echo "v0.1.0-alpha"); \
	echo "Latest tag: $$latest_tag"; \
	echo "Downloading provenance from GitHub..."; \
	mkdir -p .verify-temp; \
	curl -sSL "https://github.com/pparaujo/flow-orchestrator/releases/download/$$latest_tag/flow-orchestrator.spdx.json" -o .verify-temp/flow-orchestrator.spdx.json; \
	curl -sSL "https://github.com/pparaujo/flow-orchestrator/releases/download/$$latest_tag/flow-orchestrator.intoto.jsonl" -o .verify-temp/flow-orchestrator.intoto.jsonl; \
	echo "Verifying provenance..."; \
	slsa-verifier verify-artifact \
		--provenance-path .verify-temp/flow-orchestrator.intoto.jsonl \
		--source-uri github.com/pparaujo/flow-orchestrator \
		--source-tag $$latest_tag \
		.verify-temp/flow-orchestrator.spdx.json || echo "Verification failed - this is expected for existing releases that don't have SLSA provenance"; \
	rm -rf .verify-temp

# Help target
help:
	@echo "Available targets:"
	@echo "  all                  : Run lint, test, and build"
	@echo "  build                : Build the project"
	@echo "  test                 : Run tests with race detection"
	@echo "  test-coverage        : Run tests with coverage report for all packages"
	@echo "  test-coverage-focused: Run tests with coverage report excluding examples and generated code"
	@echo "  codecov-coverage     : Generate coverage report specifically for codecov"
	@echo "  test-core            : Run tests for core functionality only"
	@echo "  coverage-report      : Generate a focused coverage report"
	@echo "  check-coverage       : Check if coverage meets thresholds"
	@echo "  coverage-improvement : Generate a report showing which files need coverage improvement"
	@echo "  lint                 : Run linter"
	@echo "  clean                : Clean build artifacts"
	@echo "  generate-fb          : Generate FlatBuffers code"
	@echo "  examples             : Build all examples"
	@echo "  benchmark            : Run benchmarks"
	@echo "  benchmark-profile    : Generate benchmark profiles"
	@echo "  profile-cpu          : View CPU profile in browser"
	@echo "  profile-mem          : View memory profile in browser"
	@echo "  verify-slsa          : Verify SLSA provenance for the latest release"
	@echo "  property-tests       : Run property-based tests" 