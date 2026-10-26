# Look up a catalogue pack and its latest published version.
data "botyard_pack" "crm" {
  slug = "crm-assistant"
}

output "crm_latest_version" {
  value = data.botyard_pack.crm.latest.version_no
}
