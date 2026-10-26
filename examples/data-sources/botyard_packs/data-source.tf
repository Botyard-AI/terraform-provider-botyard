# List the whole pack catalogue.
data "botyard_packs" "all" {}

# Or only some packs. Slugs that match nothing come back in not_found.
data "botyard_packs" "selected" {
  slugs = ["crm-assistant", "customer-support"]
}

output "missing_packs" {
  value = data.botyard_packs.selected.not_found
}
