# Vault secrets are imported by their ID (UUID). The write-only `secret_value`
# is never stored in state and is not populated by import; set it in
# configuration and Terraform will reconcile the value on the next apply.
terraform import botyard_vault_secret.github_token 3f2504e0-4f89-11d3-9a0c-0305e82c3301
