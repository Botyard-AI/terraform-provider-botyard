# List the organization's credentials (metadata only) and pick the default LLM
# credential's id for a bot credential assignment.
data "botyard_credentials" "all" {}

locals {
  default_llm_credential_id = one([
    for c in data.botyard_credentials.all.credentials :
    c.credential_id if c.scope == "llm" && c.is_default
  ])
}
