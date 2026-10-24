# Common commands; CI runs the same steps (.github/workflows/ci.yml).
SHELL := /bin/bash
GO_TEST_COVERAGE_VERSION := v2.20.1
TEST_TIMEOUT ?= 8m
COVERAGE := go run github.com/vladopajic/go-test-coverage/v2@$(GO_TEST_COVERAGE_VERSION)
GOLANGCI_LINT_VERSION := v2.15.0
GORELEASE := golang.org/x/exp/cmd/gorelease@v0.0.0-20261014143641-f0251eb4aa3e
export GOPRIVATE := github.com/fin-assist/*

.PHONY: test test-names cover collect-unit collect-integration collect-api check-unit check-integration check-api cover-unit cover-integration cover-api cover-combined lint fmt api release-check

test: test-names ## unit, integration and API tests (Docker or TEST_DATABASE_URL), script tests, changelog format
	go mod tidy -diff
	go vet ./...
	go test -race -count=1 -timeout=$(TEST_TIMEOUT) ./...
	python3 -m unittest discover -s scripts -p 'test_*.py'
	python3 scripts/changelog.py check

test-names: ## reject tests accidentally omitted from the three selectors
	go test -race -list '^(Test|Fuzz|Example)' -timeout=8m ./... > test-names.txt
	@if grep -E '^(Test|Fuzz)' test-names.txt | grep -Ev '^(Test|Fuzz)(Unit|Integration|API).+'; then \
	  echo 'Name every test/fuzz target TestUnit..., TestIntegration..., TestAPI... or FuzzUnit..., FuzzIntegration..., FuzzAPI...' >&2; exit 1; fi

# All suites instrument the same whole module. No production files are excluded.
# Ordinary packages without tests may be reported as skipped; skipped Test events fail.
collect-unit collect-integration collect-api: test-names
	@set -euo pipefail; level='$(@:collect-%=%)'; case "$$level" in unit) prefix=Unit;; integration) prefix=Integration;; api) prefix=API;; esac; \
	rm -f "$$level.out" "$$level.jsonl"; \
	pattern="^(Test|Fuzz)$$prefix"; if [ "$$level" = api ]; then pattern="^((Test|Fuzz)API|Example)"; fi; \
	CI=true go test -json -race -count=1 -timeout=8m -coverpkg=./... \
	  -coverprofile="$$level.out" -run "$$pattern" ./... | tee "$$level.jsonl"; \
	bash scripts/check-suite.sh "$$level.jsonl" "$$level.out"

check-unit check-integration check-api:
	@set -euo pipefail; level='$(@:check-%=%)'; \
	bash scripts/check-suite.sh "$$level.jsonl" "$$level.out"; \
	$(COVERAGE) --config .testcoverage.yml --profile "$$level.out" \
	  --breakdown-file-name "$$level.coverage.txt" | tee "$$level.report.txt"

cover-unit: collect-unit
	$(MAKE) check-unit
cover-integration: collect-integration
	$(MAKE) check-integration
cover-api: collect-api
	$(MAKE) check-api

cover-combined: ## informational union; the tool merges profiles, never percentages
	@set -euo pipefail; for level in unit integration api; do bash scripts/check-suite.sh "$$level.jsonl" "$$level.out"; done
	set -euo pipefail; $(COVERAGE) --profile unit.out,integration.out,api.out \
	  --breakdown-file-name combined.coverage.txt | tee combined.report.txt

cover: collect-unit collect-integration collect-api ## collect all profiles before enforcing the three gates
	@status=0; for level in unit integration api; do $(MAKE) check-$$level || status=1; done; \
	$(MAKE) cover-combined || status=1; exit "$$status"

lint: ## golangci-lint of the pinned version
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

fmt:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) fmt ./...

api: ## exported API against the latest tag and the suggested next version (informational)
	-go run $(GORELEASE) -base=$$(git tag --list 'v*' --sort=-v:refname | head -n1 | grep . || echo none)

release-check: ## VERSION=vX.Y.Z: gorelease accepts the version and CHANGELOG.md has its section
	@test -n "$(VERSION)" || { echo "usage: make release-check VERSION=vX.Y.Z"; exit 2; }
	python3 scripts/changelog.py check
	test "$$(python3 scripts/changelog.py latest)" = "$(VERSION)"
	go run $(GORELEASE) -base=$$(git tag --list 'v*' --sort=-v:refname | head -n1 | grep . || echo none) -version=$(VERSION) \
		| tee /dev/stderr | grep -q '^$(VERSION) is a valid semantic version for this release'
