package provider

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// TestAccPackDataSources reads the live catalogue: the whole list, one pack by
// the first listed slug, that pack's latest version by slug and by id, and the
// latest version of every listed pack. It
// only reads, so it is safe against any org whose catalogue is non-empty
// (curated packs are visible to every org).
func TestAccPackDataSources(t *testing.T) {
	const cfg = `
data "botyard_packs" "all" {}

locals {
  first = data.botyard_packs.all.packs[0]
}

data "botyard_pack" "first" {
  slug = local.first.slug
}

data "botyard_pack_version" "by_slug" {
  slug = local.first.slug
}

data "botyard_pack_version" "by_id" {
  pack_version_id = data.botyard_pack.first.latest.pack_version_id
}

# Every listed pack's latest version must decode: no unknown tool runtime or
# connection kind anywhere in the live catalogue.
data "botyard_pack_version" "every" {
  # count, not for_each: terraform-plugin-testing's state shim rejects
  # string instance keys.
  count           = length(data.botyard_packs.all.packs)
  pack_version_id = data.botyard_packs.all.packs[count.index].latest.pack_version_id
}

output "decoded" {
  value = [for v in data.botyard_pack_version.every : "${v.slug} v${v.version_no}: ${length(v.skill_ids)} skills, ${length(v.tool_ids)} tools, ${length(v.connections)} connections"]
}

data "botyard_packs" "filtered" {
  slugs = [local.first.slug, "tf-acc-no-such-pack"]
}
`
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: cfg,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestMatchResourceAttr("data.botyard_packs.all", "packs.#", regexp.MustCompile(`^[1-9][0-9]*$`)),
				resource.TestCheckResourceAttrPair("data.botyard_pack.first", "pack_id", "data.botyard_pack_version.by_slug", "pack_id"),
				resource.TestCheckResourceAttrPair("data.botyard_pack.first", "latest.pack_version_id", "data.botyard_pack_version.by_slug", "pack_version_id"),
				resource.TestCheckResourceAttrPair("data.botyard_pack_version.by_slug", "content_hash", "data.botyard_pack_version.by_id", "content_hash"),
				resource.TestCheckResourceAttr("data.botyard_pack_version.by_slug", "is_latest", "true"),
				resource.TestCheckResourceAttr("data.botyard_pack_version.by_slug", "pack_archived", "false"),
				resource.TestCheckResourceAttr("data.botyard_packs.filtered", "packs.#", "1"),
				resource.TestCheckResourceAttr("data.botyard_packs.filtered", "not_found.#", "1"),
				resource.TestCheckResourceAttr("data.botyard_packs.filtered", "not_found.0", "tf-acc-no-such-pack"),
				func(s *terraform.State) error {
					fmt.Printf("live catalogue decoded: %v\n", s.RootModule().Outputs["decoded"].Value)
					return nil
				},
			),
		}},
	})
}
