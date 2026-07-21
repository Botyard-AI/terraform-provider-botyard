terraform {
  required_providers {
    botyard = {
      source  = "Botyard-AI/botyard"
      version = "~> 0.1"
    }
  }
}

# Authenticate with an organization-scoped API key. Prefer environment
# variables so the key never lands in Terraform configuration or state:
#
#   export BOTYARD_API_KEY="byk_..."
#   export BOTYARD_ORG_ID="00000000-0000-0000-0000-000000000000"
#   export BOTYARD_ENDPOINT="https://api.botyard.io" # optional; this is the default
provider "botyard" {}
