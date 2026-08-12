.PHONY: all build test bar-oracle lint clean examples benchmark generate-fb check-flatbuffers test-core coverage-report test-coverage-focused check-coverage coverage-improvement verify-slsa codecov-coverage property-tests

# Central timeout for EVERY -race target (AUD-006/AUD-052). The full -race suite
# is heavy and a rare full run has approached go's default 600s aggregate timeout
# (F-VERIFY-P112-FLAKE — suite weight, not a data race). Sized ONCE here so a
# growing suite is a one-line bump rather than a per-target hunt, and so no race
# target can silently ship without a ceiling on a genuine hang. A KILLED `go test`
# exits non-zero with zero `--- FAIL`, which looks exactly like a green — every
# race target below therefore carries this explicit ceiling.
TEST_TIMEOUT ?= 30m

# Default target
all: generate-fb lint test build

# Build the project
build: generate-fb
	go build ./...

# Run tests
# -timeout 30m: the full -race suite is heavy; a rare intermittent full-run has
# approached go's default 600s -timeout (F-VERIFY-P112-FLAKE — zero data race,
# suite weight). The explicit 30m headroom keeps the release gate from stranding
# on the aggregate timeout while staying a hard ceiling on a genuine hang.
test: generate-fb
	go test -race -timeout $(TEST_TIMEOUT) ./...

# The BAR-M23 oracle's verdict WITH ITS BOUNDS (M23 118B-9). `make test` above shows
# neither: `go test` DISCARDS a passing package's entire binary output — measured on
# go1.25.1 with controls, and it is not only t.Logf but fmt.Println, os.Stdout, os.Stderr
# and TestMain too — so on a green the oracle's population bound and arm-availability
# table print ZERO times, and a phase citing "oracle green" never meets them.
#
# THIS is the invocation that prints them. -v is load-bearing rather than decoration, and
# TestBARM23_BoundsHaveAnInvocationThatPrintsThem reds if this target loses it or loses
# its selection.
bar-oracle: generate-fb
	go test -timeout 30m -count=1 -v -run 'TestBARM23_' ./pkg/workflow/

# AUD-002 retired the `adversarial_copyclass` tag, its script and the `test-tagged` target. The tag
# existed ONLY to quarantine `go vet`'s copylocks diagnostic on `cp := *dag` — the first half of the
# copied-mutex-wedge finding. The fix makes DAG.mu a *sync.RWMutex, so a value copy no longer trips
# copylocks AND no longer inherits a locked mutex; the copy-class tests moved into the default build
# (seal_adversarial_117_copyclass_test.go) and run under `make test` / the -race gate like any other.

# Run property-based tests
property-tests: generate-fb
	./scripts/testing/run_property_tests.sh

# Run tests with coverage
test-coverage: generate-fb
	go test -race -timeout $(TEST_TIMEOUT) -coverprofile=coverage.tmp.txt -covermode=atomic ./...
	mv coverage.tmp.txt coverage.txt
	go tool cover -html=coverage.txt -o coverage.html

# Run tests with coverage for core packages only (excluding examples, benchmarks, and generated code)
# NOTE: CI's coverage job calls this target. It MUST carry -timeout (AUD-006): a
# killed race run writes a PARTIAL profile and exits non-zero with zero --- FAIL,
# which a coverage gate reads as a truncated-but-"green" result. The profile is
# written to a temp file and renamed only on success, so a killed run cannot leave
# a truncated coverage-focused.txt standing in for a complete one.
test-coverage-focused: generate-fb
	go test -race -timeout $(TEST_TIMEOUT) -coverprofile=coverage-focused.tmp.txt -covermode=atomic `go list ./... | grep -v "examples\|fb\|benchmark"`
	mv coverage-focused.tmp.txt coverage-focused.txt
	go tool cover -html=coverage-focused.txt -o coverage-focused.html

# Generate coverage report specifically for codecov
codecov-coverage: generate-fb
	@echo "Generating coverage report for codecov..."
	@# Run tests for all packages
	@go test -race -timeout $(TEST_TIMEOUT) ./...
	@# Generate coverage report for relevant packages only (temp → rename on success)
	@go test -race -timeout $(TEST_TIMEOUT) -coverprofile=coverage.tmp.txt -covermode=atomic `go list ./... | grep -v "examples\|fb\|benchmark"` && mv coverage.tmp.txt coverage.txt
	@echo "Coverage report generated at coverage.txt"
	@echo "Coverage by priority level:"
	@echo "Critical (pkg/workflow): $(shell go tool cover -func=coverage.txt | grep "pkg/workflow" | grep -v "internal/workflow/fb" | grep total | awk '{print $$3}')"
	@echo "High (arena, memory): $(shell go tool cover -func=coverage.txt | grep "internal/workflow/arena\|internal/workflow/memory" | grep total | awk '{print $$3}')"
	@echo "Medium (metrics, utils): $(shell go tool cover -func=coverage.txt | grep "internal/workflow/metrics\|internal/workflow/utils" | grep total | awk '{print $$3}')"

# Run tests for core functionality only
test-core: generate-fb
	go test -race -timeout $(TEST_TIMEOUT) -coverprofile=coverage-core.tmp.txt -covermode=atomic ./pkg/workflow ./internal/workflow/arena ./internal/workflow/memory
	mv coverage-core.tmp.txt coverage-core.txt
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

# Pinned golangci-lint version. The repo's .golangci.yml uses the v2 schema, so
# lint must run under v2. Pinning + `go run` makes lint reproducible regardless of
# which (if any) golangci-lint binary a developer has installed locally.
GOLANGCI_LINT_VERSION ?= v2.12.2

# Run linter
lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run --config .golangci.yml

# Clean build artifacts
clean:
	rm -f coverage*.txt coverage*.html
	rm -rf ./bin
	rm -rf internal/workflow/fb

# FlatBuffers related targets
check-flatbuffers:
	./scripts/tools/check_flatbuffers.sh

generate-fb: check-flatbuffers
	mkdir -p internal/workflow/fb
	flatc --go -o internal/workflow/fb pkg/workflow/schema/workflow_data.fbs

# Run examples
# Run the examples. This target said "Running example:" and ran `go build` for nine
# milestones; CI carried the same lie. See the comment on the CI step.
#
# The durable state is cleared first, and that is not tidiness: these examples persist
# their journals under their own directories, so a SECOND run resumes a completed
# workflow and skips every node — it finishes in milliseconds having executed nothing.
# A target that silently no-ops on re-run is the same class of lie this target is being
# fixed to remove. CI gets a fresh checkout and never sees it; a developer would.
examples:
	@for example in $$(find examples -name "main.go" -not -path "*/\.*" | sort); do \
		dir=$$(dirname $$example); \
		echo "Running example: $$dir"; \
		rm -rf $$dir/workflow_data.json $$dir/workflow_data.fb $$dir/api_workflow_state; \
		(cd $$dir && go run .) || exit 1; \
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
	curl -sSL "https://github.com/ppcavalcante/flow-orchestrator/releases/download/$$latest_tag/flow-orchestrator.spdx.json" -o .verify-temp/flow-orchestrator.spdx.json; \
	curl -sSL "https://github.com/ppcavalcante/flow-orchestrator/releases/download/$$latest_tag/flow-orchestrator.intoto.jsonl" -o .verify-temp/flow-orchestrator.intoto.jsonl; \
	echo "Verifying provenance..."; \
	slsa-verifier verify-artifact \
		--provenance-path .verify-temp/flow-orchestrator.intoto.jsonl \
		--source-uri github.com/ppcavalcante/flow-orchestrator \
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