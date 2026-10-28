package provider

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// TestAccBotPack creates a bot from the first catalogue pack, checks the next
// plan is empty, imports it, then removes the block (uninstall, bot kept).
// The bot is destroyed at the end. Connections are left to the API default.
func TestAccBotPack(t *testing.T) {
	name := "tf-acc-pack-" + acctest.RandString(6)
	cfg := func(withPack bool) string {
		pack := ""
		if withPack {
			pack = `pack = { pack_version_id = data.botyard_packs.all.packs[0].latest.pack_version_id }`
		}
		return fmt.Sprintf(`
data "botyard_packs" "all" {}

resource "botyard_bot" "p" {
  name = %q
  %s
}
`, name, pack)
	}
	const addr = "botyard_bot.p"
	uuid := regexp.MustCompile(`^[0-9a-f-]{36}$`)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: cfg(true),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestMatchResourceAttr(addr, "pack.bot_pack_install_id", uuid),
					resource.TestCheckResourceAttrPair(addr, "pack.pack_version_id", "data.botyard_packs.all", "packs.0.latest.pack_version_id"),
					resource.TestCheckResourceAttrPair(addr, "pack.pack_slug", "data.botyard_packs.all", "packs.0.slug"),
					resource.TestCheckNoResourceAttr(addr, "avatar_url"),
				),
			},
			{
				Config: cfg(true),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				ResourceName: addr,
				ImportState:  true,
				ImportStateIdFunc: func(s *terraform.State) (string, error) {
					return s.RootModule().Resources[addr].Primary.Attributes["slug"], nil
				},
				ImportStateVerify:                    true,
				ImportStateVerifyIdentifierAttribute: "slug",
				// Live telemetry moves between reads.
				ImportStateVerifyIgnore: []string{"health_status", "updated_at", "desired_state", "onboarding_state", "config_generation", "pack.pending_connections"},
			},
			{
				Config: cfg(false),
				Check:  resource.TestCheckNoResourceAttr(addr, "pack.bot_pack_install_id"),
			},
		},
	})
}
