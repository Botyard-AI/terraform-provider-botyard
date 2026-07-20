# Terraform Provider for Botyard

Manage [Botyard](https://botyard.io) platform resources — bots, skills,
workforces, credentials, and MCP servers — as code.

> **Status: early scaffold.** This is the provider skeleton (task #785 of the
> Terraform Provider epic). It ships the provider configuration, a generated API
> client, authentication, and a single `botyard_bot` data source. Managed
> resources are added incrementally.

## Usage

```hcl
terraform {
  required_providers {
    botyard = {
      source = "Botyard-AI/botyard"
    }
  }
}

provider "botyard" {
  # endpoint defaults to https://api.botyard.io
  # api_key and org_id are best supplied via environment variables:
  #   BOTYARD_API_KEY, BOTYARD_ORG_ID, BOTYARD_ENDPOINT
}

data "botyard_bot" "example" {
  slug = "my-bot"
}

output "bot_id" {
  value = data.botyard_bot.example.id
}
```

### Authentication

The provider authenticates with an **organization-scoped Botyard API key**
(`byk_...`), created by an org owner. Configure it via the `api_key` provider
attribute or, preferably, the `BOTYARD_API_KEY` environment variable so the key
never lands in Terraform configuration or state. Every resource is scoped to the
organization set in `org_id` (or `BOTYARD_ORG_ID`).

| Setting    | Attribute  | Environment variable | Default                    |
| ---------- | ---------- | -------------------- | -------------------------- |
| API base   | `endpoint` | `BOTYARD_ENDPOINT`   | `https://api.botyard.io`   |
| API key    | `api_key`  | `BOTYARD_API_KEY`    | —                          |
| Org ID     | `org_id`   | `BOTYARD_ORG_ID`     | —                          |

## Development

Requires Go (see `go.mod`), plus `python3` and `node`/`npx` to regenerate the
API client.

```sh
make build      # compile
make test       # unit tests (hermetic)
make fmtcheck   # gofmt check
make vet        # go vet
make testacc    # acceptance tests (needs a reachable API; see below)
```

### Generated API client

`internal/client/botyard.gen.go` is generated from the vendored public Botyard
OpenAPI spec (`internal/client/openapi.json`) — do not edit it by hand.

```sh
make sync-spec BOTYARD_MONOREPO=~/src/botyard   # refresh the vendored spec
make generate                                   # regenerate the client
```

The generation pipeline (`scripts/generate-client.sh`) normalizes the FastAPI
OpenAPI **3.1** spec to a form oapi-codegen accepts: it fixes 3.1 constructs,
prunes the spec to the covered operation tags, down-converts 3.1 → 3.0, and runs
oapi-codegen. Client coverage grows by widening `KEEP_TAGS` in that script as
resources are added.

### Acceptance tests

Acceptance tests (`TF_ACC=1`) run real `terraform apply/plan/destroy` against a
Botyard API. The recommended setup is a hermetic local API + Postgres stack (no
Kubernetes required — bot records are created without a live provisioner). The
full harness and release pipeline land in a follow-up task.

## License

[MPL-2.0](./LICENSE).
