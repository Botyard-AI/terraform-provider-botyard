BINARY := terraform-provider-botyard

# Pinned tool versions for documentation generation. Keep in sync with
# .github/workflows/test.yml so `make docs` mirrors the deterministic CI path.
TFPLUGINDOCS_VERSION := v0.25.0
TERRAFORM_VERSION := 1.9.8

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

# Regenerate the Terraform Registry documentation (docs/) from the provider
# schema and the examples/ directory using tfplugindocs. --tf-version pins the
# terraform binary tfplugindocs downloads so example formatting is deterministic
# and matches CI. Run this whenever the schema or examples change and commit the
# result; CI fails if docs/ drifts from the schema.
docs:
	go run github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs@$(TFPLUGINDOCS_VERSION) \
		generate --provider-name botyard --rendered-provider-name Botyard --tf-version $(TERRAFORM_VERSION)

# Verify docs/ is in sync with the current schema + examples. Regenerates and
# fails if anything changed. Uses `git status --porcelain` (not `git diff`) so
# that newly generated but untracked pages also count as drift. Mirrors CI.
docs-check: docs
	@drift="$$(git status --porcelain -- docs/)"; \
	if [ -n "$$drift" ]; then \
		echo "docs/ is out of date; run 'make docs' and commit the result:"; \
		echo "$$drift"; exit 1; \
	fi

.PHONY: default build install test testacc generate sync-spec fmt fmtcheck vet docs docs-check
