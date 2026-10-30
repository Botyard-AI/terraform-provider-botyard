package provider

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

// TestBotSchedulesDataSource lists every schedule on the bot, including ones
// Terraform did not create: a prompt schedule the bot made itself, a command
// schedule, and one whose timing kind (`event`) this provider cannot model,
// which stays listed with only spec_kind set. next_fire_at is exposed here,
// and is null for a paused schedule.
func TestBotSchedulesDataSource(t *testing.T) {
	api, srv := newFakeScheduleAPI(t)
	seed := func(id, kind, spec, action string, enabled bool) {
		norm, actionKind, err := normalizeFakeAction(json.RawMessage(action))
		if err != nil {
			t.Fatal(err)
		}
		var persisted map[string]any
		if err := json.Unmarshal([]byte(spec), &persisted); err != nil {
			t.Fatal(err)
		}
		api.schedules[id] = &fakeSchedule{
			id: id, conversationID: "conv-" + id, enabled: enabled, catchup: "skip",
			specKind: client.ScheduleSpecKind(kind), spec: persisted, timezone: "Europe/Copenhagen",
			actionKind: actionKind, action: norm,
		}
		api.order = append(api.order, id)
	}
	seed("sch-bot-made", "cron", `{"cron":"30 7 * * *"}`, `{"kind":"prompt","prompt":"Daily digest"}`, true)
	seed("sch-cmd", "interval", `{"interval_seconds":900,"anchor":"2026-01-01T00:00:00+00:00"}`,
		`{"kind":"command","command":"./poll.sh","log_path":"/tmp/poll.log"}`, false)
	seed("sch-event", "event", `{"event":"github.push"}`, `{"kind":"prompt","prompt":"New push"}`, true)

	const addr = "data.botyard_bot_schedules.all"
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: fmt.Sprintf(`
provider "botyard" {
  endpoint = %q
  api_key  = "byk_test"
  org_id   = "org-1"
}

data "botyard_bot_schedules" "all" {
  bot_slug = %q
}
`, srv.URL, fakeScheduleBot),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(addr, "schedules.#", "3"),

				resource.TestCheckResourceAttr(addr, "schedules.0.id", "sch-bot-made"),
				resource.TestCheckResourceAttr(addr, "schedules.0.spec_kind", "cron"),
				resource.TestCheckResourceAttr(addr, "schedules.0.cron.cron", "30 7 * * *"),
				resource.TestCheckNoResourceAttr(addr, "schedules.0.interval"),
				resource.TestCheckResourceAttr(addr, "schedules.0.timezone", "Europe/Copenhagen"),
				resource.TestCheckResourceAttr(addr, "schedules.0.action_kind", "prompt"),
				resource.TestCheckResourceAttr(addr, "schedules.0.prompt", "Daily digest"),
				resource.TestCheckNoResourceAttr(addr, "schedules.0.command"),
				resource.TestCheckResourceAttr(addr, "schedules.0.next_fire_at", "2026-11-02T09:00:00Z"),

				resource.TestCheckResourceAttr(addr, "schedules.1.interval.interval_seconds", "900"),
				resource.TestCheckResourceAttr(addr, "schedules.1.interval.anchor", "2026-01-01T00:00:00Z"),
				resource.TestCheckResourceAttr(addr, "schedules.1.action_kind", "command"),
				resource.TestCheckResourceAttr(addr, "schedules.1.command.command", "./poll.sh"),
				resource.TestCheckResourceAttr(addr, "schedules.1.command.overlap_policy", "skip"),
				resource.TestCheckResourceAttr(addr, "schedules.1.command.log_path", "/tmp/poll.log"),
				resource.TestCheckNoResourceAttr(addr, "schedules.1.prompt"),
				resource.TestCheckResourceAttr(addr, "schedules.1.enabled", "false"),
				resource.TestCheckNoResourceAttr(addr, "schedules.1.next_fire_at"),

				resource.TestCheckResourceAttr(addr, "schedules.2.spec_kind", "event"),
				resource.TestCheckNoResourceAttr(addr, "schedules.2.cron"),
				resource.TestCheckNoResourceAttr(addr, "schedules.2.interval"),
				resource.TestCheckNoResourceAttr(addr, "schedules.2.rrule"),
				resource.TestCheckResourceAttr(addr, "schedules.2.prompt", "New push"),
			),
		}},
	})
}
