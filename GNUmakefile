BINARY := terraform-provider-botyard

default: build

# Compile the provider and all packages.
build:
	go build ./...

# Install the provider into $GOPATH/bin for local terraform testing.
install:
	go install .

# Unit tests (fast, hermetic).
test:
	go test ./... -count=1

# Acceptance tests. Requires a reachable Botyard API and:
#   BOTYARD_ENDPOINT, BOTYARD_API_KEY, BOTYARD_ORG_ID
# See README for the recommended hermetic local API+Postgres stack.
testacc:
	TF_ACC=1 go test ./... -count=1 -timeout 120m

# Regenerate the API client from the vendored OpenAPI spec.
# (Runs scripts/generate-client.sh via the //go:generate directive.)
generate:
	go generate ./...

# Refresh the vendored OpenAPI spec from a local Botyard monorepo checkout,
# then run `make generate`. Example:
#   make sync-spec BOTYARD_MONOREPO=~/src/botyard
sync-spec:
	@test -n "$(BOTYARD_MONOREPO)" || { echo "set BOTYARD_MONOREPO=/path/to/botyard checkout"; exit 1; }
	cp "$(BOTYARD_MONOREPO)/docs-site/content/api/openapi.json" internal/client/openapi.json
	@echo "spec synced; run 'make generate' to regenerate the client."

fmt:
	gofmt -w main.go internal/

fmtcheck:
	@test -z "$$(gofmt -l main.go internal/)" || { echo "gofmt needed:"; gofmt -l main.go internal/; exit 1; }

vet:
	go vet ./...

.PHONY: default build install test testacc generate sync-spec fmt fmtcheck vet
