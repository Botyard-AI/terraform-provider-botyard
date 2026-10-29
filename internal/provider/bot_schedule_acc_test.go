package provider

import (
	"fmt"
	"net/http"
	"os"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// TestAccBotSchedule drives botyard_bot_schedule against the live API: create
// a prompt schedule on a fresh bot with an adopted conversation, re-plan clean,
// import, switch timing kind in place, pause it out of band and see the next
// apply resume it, and list it through botyard_bot_schedules. The bot (and
// with it the schedule) is destroyed at the end.
func TestAccBotSchedule(t *testing.T) {
	name := "tf-acc-sched-" + acctest.RandString(6)
	cfg := func(timing, extra string) string {
		return fmt.Sprintf(`
resource "botyard_bot" "b" {
  name = %q
}

resource "botyard_bot_schedule" "s" {
  bot_slug = botyard_bot.b.slug
  %s
  prompt   = "Acceptance-test wake; ignore."
  %s
}

data "botyard_bot_schedules" "all" {
  bot_slug   = botyard_bot.b.slug
  depends_on = [botyard_bot_schedule.s]
}
`, name, timing, extra)
	}
	const addr = "botyard_bot_schedule.s"
	cron := cfg(`cron = { cron = "0 9 * * 1-5", timezone = "Europe/Copenhagen" }`, "")
	// `+00:00` is how the API echoes an anchor, and a spelling Go never
	// produces itself, so a clean re-plan proves the configured spelling is kept.
	interval := cfg(`interval = { interval_seconds = 86400, anchor = "2026-01-01T09:00:00+00:00" }`, `title = "acc"`)
	uuid := regexp.MustCompile(`^[0-9a-f-]{36}$`)
	empty := resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}
	inPlace := resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
		plancheck.ExpectResourceAction(addr, plancheck.ResourceActionUpdate),
	}}

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: cron,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestMatchResourceAttr(addr, "id", uuid),
					resource.TestMatchResourceAttr(addr, "conversation_id", uuid),
					resource.TestCheckResourceAttr(addr, "enabled", "true"),
					resource.TestCheckNoResourceAttr(addr, "title"),
				),
			},
			{Config: cron, ConfigPlanChecks: empty},
			{
				ResourceName:      addr,
				ImportState:       true,
				ImportStateIdFunc: accScheduleImportID,
				ImportStateVerify: true,
			},
			{
				Config:           interval,
				ConfigPlanChecks: inPlace,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(addr, "cron"),
					resource.TestCheckResourceAttr(addr, "interval.anchor", "2026-01-01T09:00:00+00:00"),
					resource.TestCheckResourceAttr(addr, "title", "acc"),
				),
			},
			{Config: interval, ConfigPlanChecks: empty},
			{
				// Paused outside Terraform: Terraform owns enabled, so it plans back.
				PreConfig:        func() { accPauseSchedule(t) },
				Config:           interval,
				ConfigPlanChecks: inPlace,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "enabled", "true"),
					resource.TestCheckResourceAttr("data.botyard_bot_schedules.all", "schedules.#", "1"),
					resource.TestCheckResourceAttrPair("data.botyard_bot_schedules.all", "schedules.0.id", addr, "id"),
					resource.TestCheckResourceAttr("data.botyard_bot_schedules.all", "schedules.0.spec_kind", "interval"),
					resource.TestMatchResourceAttr("data.botyard_bot_schedules.all", "schedules.0.next_fire_at", regexp.MustCompile(`^20\d\d-`)),
				),
			},
		},
	})
}

// accScheduleState remembers the live schedule between steps for the
// out-of-band pause, which needs its bot slug and id.
var accScheduleState struct{ slug, id string }

func accScheduleImportID(s *terraform.State) (string, error) {
	rs, ok := s.RootModule().Resources["botyard_bot_schedule.s"]
	if !ok {
		return "", fmt.Errorf("botyard_bot_schedule.s not in state")
	}
	accScheduleState.slug, accScheduleState.id = rs.Primary.Attributes["bot_slug"], rs.Primary.ID
	return rs.Primary.Attributes["bot_slug"] + "/" + rs.Primary.ID, nil
}

// accPauseSchedule pauses the schedule through the API, the way the app (or
// an auto-pause) would.
func accPauseSchedule(t *testing.T) {
	t.Helper()
	endpoint := os.Getenv("BOTYARD_ENDPOINT")
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	url := fmt.Sprintf("%s/v1/orgs/%s/bots/%s/schedules/%s/pause",
		endpoint, os.Getenv("BOTYARD_ORG_ID"), accScheduleState.slug, accScheduleState.id)
	// The pause endpoint is deliberately not in the generated client (the
	// provider never pauses), so the test calls it directly. The URL is built
	// from the operator's own BOTYARD_ENDPOINT, not untrusted input.
	req, err := http.NewRequest(http.MethodPost, url, nil) //nolint:gosec // G704: test-only, operator-supplied endpoint
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+os.Getenv("BOTYARD_API_KEY"))
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: test-only, operator-supplied endpoint
	if err != nil {
		t.Fatalf("pausing schedule: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pausing schedule: HTTP %d", resp.StatusCode)
	}
}
