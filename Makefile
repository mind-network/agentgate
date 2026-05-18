.PHONY: test lint build clean migrate-up migrate-down test-e2e test-conformance

test:
	go test ./...

LINT_ENV := GOCACHE=$(CURDIR)/.cache/go-build GOLANGCI_LINT_CACHE=$(CURDIR)/.cache/golangci-lint

lint:
	@fmt_files="$$(gofmt -l $$(git ls-files '*.go'))"; \
	if [ -n "$$fmt_files" ]; then \
		printf 'gofmt required:\n%s\n' "$$fmt_files"; \
		exit 1; \
	fi
	$(LINT_ENV) golangci-lint run ./...

build:
	go build -o aicg-gw ./cmd/aicg-gw
	go build -o aicg-lp ./cmd/aicg-lp
	go build -o aicg-db ./cmd/aicg-db

migrate-up:
	go run ./cmd/aicg-db up

migrate-down:
	go run ./cmd/aicg-db down

test-e2e:
	go test -tags=e2e ./tests/e2e/...

test-conformance:
	go test -v -timeout 120s ./tests/conformance/...

clean:
	rm -f aicg-gw aicg-lp aicg-db
