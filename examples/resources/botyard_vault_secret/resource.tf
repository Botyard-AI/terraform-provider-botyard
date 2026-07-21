# A Runtime Vault secret (secret policy): the encrypted value plus its access
# rules. `secret_value` is write-only and requires Terraform 1.11 or later.

variable "github_token" {
  type      = string
  sensitive = true
}

# Grant access to specific bots.
resource "botyard_vault_secret" "github_token" {
  key_path     = "github.tokens.read_only"
  display_name = "GitHub read-only token"
  description  = "Read-only PAT used by CI bots."

  # Write-only: sent to the API but never stored in Terraform state. Only a
  # one-way SHA-256 fingerprint is kept so a value change triggers rotation.
  secret_value = var.github_token

  max_ttl_seconds = 900

  # Authoritative set of bots allowed to lease this secret.
  bot_ids = [
    "3f2504e0-4f89-11d3-9a0c-0305e82c3301",
  ]
}

# Grant access to every bot in the organization instead of an explicit set.
resource "botyard_vault_secret" "shared_config" {
  key_path       = "shared.config.endpoint"
  display_name   = "Shared config endpoint"
  sensitivity    = "plain"
  secret_value   = "https://config.example.com"
  allow_all_bots = true
}
