package provider

import (
	"encoding/json"
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const scheduleAddr = "botyard_bot_schedule.s"

func scheduleImportID(s *terraform.State) (string, error) {
	rs, ok := s.RootModule().Resources[scheduleAddr]
	if !ok {
		return "", fmt.Errorf("%s not in state", scheduleAddr)
	}
	return fakeScheduleBot + "/" + rs.Primary.ID, nil
}

// TestBotSchedule_PromptLifecycle drives real Terraform core through the
// prompt-action acceptance criteria of #4490: an omitted conversation_id adopts
// the auto-created one with no diff, each edit sends a PATCH holding only what
// changed, removing the title clears it, switching timing kind updates in
// place, an anchor written with `Z` survives the API's `+00:00` echo, an
// auto-pause is planned back, import re-plans clean, and a remote delete drops
// the schedule from state.
func TestBotSchedule_PromptLifecycle(t *testing.T) {
	api, srv := newFakeScheduleAPI(t)
	cron := scheduleConfig(srv.URL, `
  cron   = { cron = "0 9 * * 1-5" }
  prompt = "Morning check-in"
`)
	titled := scheduleConfig(srv.URL, `
  title  = "Morning"
  cron   = { cron = "0 9 * * 1-5" }
  prompt = "Morning check-in"
`)
	retargeted := scheduleConfig(srv.URL, `
  conversation_id = "conv-elsewhere"
  cron            = { cron = "0 9 * * 1-5" }
  prompt          = "Morning check-in"
`)
	interval := scheduleConfig(srv.URL, `
  interval = { interval_seconds = 3600, anchor = "2026-01-01T09:00:00Z" }
  prompt   = "Morning check-in"
`)
	// The same instant spelled the way the API echoes it. Go formats any
	// zero-offset instant with `Z`, so only a spelling Go would not produce
	// itself proves the prior spelling is kept.
	respelled := scheduleConfig(srv.URL, `
  interval = { interval_seconds = 3600, anchor = "2026-01-01T09:00:00+00:00" }
  prompt   = "Morning check-in"
`)
	rrule := scheduleConfig(srv.URL, `
  rrule  = { rrule = "FREQ=WEEKLY;INTERVAL=2;BYDAY=MO", timezone = "Europe/Copenhagen" }
  prompt = "Fortnightly review"
`)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: cron,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(scheduleAddr, "id", "sch-1"),
					resource.TestCheckResourceAttr(scheduleAddr, "conversation_id", "conv-auto-1"),
					resource.TestCheckResourceAttr(scheduleAddr, "cron.timezone", "UTC"),
					resource.TestCheckResourceAttr(scheduleAddr, "enabled", "true"),
					resource.TestCheckResourceAttr(scheduleAddr, "catchup_policy", "skip"),
					resource.TestCheckNoResourceAttr(scheduleAddr, "title"),
					resource.TestCheckNoResourceAttr(scheduleAddr, "next_fire_at"),
					func(*terraform.State) error {
						body := api.creates[0]
						if !isNull(body["conversation_id"]) {
							return fmt.Errorf("create sent conversation_id %s, want null so the API creates one", body["conversation_id"])
						}
						if got := string(body["action"]); got != `{"kind":"prompt","prompt":"Morning check-in"}` {
							return fmt.Errorf("create action = %s", got)
						}
						return nil
					},
					api.wantPatchKeys(),
				),
			},
			{Config: cron, PlanOnly: true},
			{
				Config: titled,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(scheduleAddr, "title", "Morning"),
					api.wantPatchKeys([]string{"title"}),
				),
			},
			{
				// Removing the title clears it: an explicit null, not an omission.
				Config: cron,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(scheduleAddr, "title"),
					api.wantPatchKeys([]string{"title"}, []string{"title"}),
					func(*terraform.State) error {
						if got := api.lastPatch("title"); got != "null" {
							return fmt.Errorf("title PATCH value = %s, want null", got)
						}
						return nil
					},
				),
			},
			{
				Config: retargeted,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(scheduleAddr, "conversation_id", "conv-elsewhere"),
					api.wantPatchKeys([]string{"title"}, []string{"title"}, []string{"conversation_id"}),
				),
			},
			// Dropping conversation_id from configuration keeps the current target.
			{Config: cron, PlanOnly: true},
			{
				Config: interval,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(scheduleAddr, "cron"),
					resource.TestCheckResourceAttr(scheduleAddr, "interval.interval_seconds", "3600"),
					resource.TestCheckResourceAttr(scheduleAddr, "interval.anchor", "2026-01-01T09:00:00Z"),
					resource.TestCheckResourceAttr(scheduleAddr, "id", "sch-1"),
					api.wantPatchKeys([]string{"title"}, []string{"title"}, []string{"conversation_id"}, []string{"schedule"}),
					func(*terraform.State) error {
						if got := api.only().specJSON(); got != `{"anchor":"2026-01-01T09:00:00+00:00","interval_seconds":3600}` {
							return fmt.Errorf("fake stored spec %s; the echo must differ in spelling for this test to mean anything", got)
						}
						return nil
					},
				),
			},
			{Config: interval, PlanOnly: true},
			{
				// Re-spelling the anchor names the same instant: no PATCH, and the
				// state keeps the configured spelling rather than Go's `Z`.
				Config: respelled,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(scheduleAddr, "interval.anchor", "2026-01-01T09:00:00+00:00"),
					api.wantPatchKeys([]string{"title"}, []string{"title"}, []string{"conversation_id"}, []string{"schedule"}),
				),
			},
			{Config: respelled, PlanOnly: true},
			{
				// Timing and action change together: one PATCH carrying both.
				Config: rrule,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(scheduleAddr, "rrule.timezone", "Europe/Copenhagen"),
					resource.TestCheckResourceAttr(scheduleAddr, "prompt", "Fortnightly review"),
					resource.TestCheckResourceAttr(scheduleAddr, "id", "sch-1"),
					api.wantPatchKeys([]string{"title"}, []string{"title"}, []string{"conversation_id"},
						[]string{"schedule"}, []string{"action", "schedule"}),
				),
			},
			{
				// An auto-pause (or a pause in the app) is drift: Terraform owns enabled.
				PreConfig:          api.set(func(a *fakeScheduleAPI) { a.only().enabled = false }),
				Config:             rrule,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				Config: rrule,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(scheduleAddr, "enabled", "true"),
					api.wantPatchKeys([]string{"title"}, []string{"title"}, []string{"conversation_id"},
						[]string{"schedule"}, []string{"action", "schedule"}, []string{"enabled"}),
				),
			},
			{
				ResourceName:      scheduleAddr,
				ImportState:       true,
				ImportStateIdFunc: scheduleImportID,
				ImportStateVerify: true,
				Config:            rrule,
			},
			{
				// Deleted outside Terraform: refresh drops it and the plan recreates it.
				PreConfig:          api.set(func(a *fakeScheduleAPI) { a.remove("sch-1") }),
				Config:             rrule,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

const commandSchedule = `
  cron = { cron = "*/15 * * * *" }
  command = {
    command           = "./scripts/sync.sh"
    cwd               = "jobs"
    timeout_seconds   = 600
    log_path          = "/home/openclaw/logs/sync.log"
    concurrency_group = "sync"
    overlap_policy    = "queue"
    secret_env        = [{ name = "SYNC_TOKEN", secret_key_path = "sync.token" }]
    overridable_env   = ["SYNC_SINCE"]
    exit_codes        = { "0" = "success", "75" = "deferred", "*" = "failure" }
    exit_code_labels  = { "75" = "Waiting for upstream" }
    window            = { duration_seconds = 900, recheck_seconds = 300 }
    on_outcome = {
      failure = {
        retry = { max = 2 }
        then  = { wake_bot = { message = "Sync failed: {exit_code}", include_log_tail = 20 } }
      }
      late = { notify_humans = { mode = "every_run" } }
    }
  }
`

// TestBotSchedule_CommandAction covers a command action with every nested
// shape: the API echoes every default the configuration left out, and the
// re-plan is still clean. An edit inside the command sends only `action`,
// import re-plans clean, and switching back to a prompt is an in-place PATCH.
func TestBotSchedule_CommandAction(t *testing.T) {
	api, srv := newFakeScheduleAPI(t)
	cmd := scheduleConfig(srv.URL, commandSchedule)
	edited := scheduleConfig(srv.URL, regexp.MustCompile(`max = 2`).ReplaceAllString(commandSchedule, `max = 3, backoff_seconds = 30`))
	prompt := scheduleConfig(srv.URL, `
  cron   = { cron = "*/15 * * * *" }
  prompt = "Run the sync by hand"
`)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: cmd,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(scheduleAddr, "prompt"),
					resource.TestCheckResourceAttr(scheduleAddr, "command.overlap_policy", "queue"),
					resource.TestCheckResourceAttr(scheduleAddr, "command.on_outcome.failure.retry.backoff_seconds", "0"),
					resource.TestCheckResourceAttr(scheduleAddr, "command.on_outcome.failure.then.pause", "false"),
					resource.TestCheckResourceAttr(scheduleAddr, "command.on_outcome.late.notify_humans.recipients.#", "0"),
					resource.TestCheckResourceAttr(scheduleAddr, "command.on_outcome.late.pause", "false"),
					func(*terraform.State) error {
						var sent struct {
							Kind          string          `json:"kind"`
							OverlapPolicy string          `json:"overlap_policy"`
							Window        json.RawMessage `json:"window"`
						}
						if err := json.Unmarshal(api.creates[0]["action"], &sent); err != nil {
							return err
						}
						if sent.Kind != "command" || sent.OverlapPolicy != "queue" || isNull(sent.Window) {
							return fmt.Errorf("create action = %s", api.creates[0]["action"])
						}
						return nil
					},
				),
			},
			{Config: cmd, PlanOnly: true},
			{
				ResourceName:      scheduleAddr,
				ImportState:       true,
				ImportStateIdFunc: scheduleImportID,
				ImportStateVerify: true,
				Config:            cmd,
			},
			{
				Config: edited,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(scheduleAddr, "command.on_outcome.failure.retry.max", "3"),
					resource.TestCheckResourceAttr(scheduleAddr, "command.on_outcome.failure.retry.backoff_seconds", "30"),
					api.wantPatchKeys([]string{"action"}),
				),
			},
			{Config: edited, PlanOnly: true},
			{
				Config: prompt,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(scheduleAddr, "prompt", "Run the sync by hand"),
					resource.TestCheckNoResourceAttr(scheduleAddr, "command"),
					resource.TestCheckResourceAttr(scheduleAddr, "id", "sch-1"),
					api.wantPatchKeys([]string{"action"}, []string{"action"}),
				),
			},
			{Config: prompt, PlanOnly: true},
		},
	})
}

// TestBotSchedule_CommandPermission covers a key whose role lacks
// schedule:schedule_command.{create,update}: creating or switching to a command
// fails with a diagnostic naming the permission, while a prompt schedule, and
// an edit that leaves an existing command untouched, still work because the
// PATCH does not resend the action.
func TestBotSchedule_CommandPermission(t *testing.T) {
	api, srv := newFakeScheduleAPI(t)
	cmd := scheduleConfig(srv.URL, `
  cron    = { cron = "0 * * * *" }
  command = { command = "./run.sh" }
`)
	cmdTitled := scheduleConfig(srv.URL, `
  title   = "Hourly"
  cron    = { cron = "0 * * * *" }
  command = { command = "./run.sh" }
`)
	prompt := scheduleConfig(srv.URL, `
  cron   = { cron = "0 * * * *" }
  prompt = "Hourly"
`)
	deny := api.set(func(a *fakeScheduleAPI) { a.denyCommand = true })
	allow := api.set(func(a *fakeScheduleAPI) { a.denyCommand = false })

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				PreConfig:   deny,
				Config:      cmd,
				ExpectError: regexp.MustCompile(`(?s)Missing permission for a command schedule.*schedule:schedule_command\.create`),
			},
			{Config: prompt, Check: resource.TestCheckResourceAttr(scheduleAddr, "prompt", "Hourly")},
			{
				Config:      cmd,
				ExpectError: regexp.MustCompile(`(?s)Missing permission for a command schedule.*schedule:schedule_command\.update`),
			},
			{PreConfig: allow, Config: cmd, Check: resource.TestCheckResourceAttr(scheduleAddr, "command.command", "./run.sh")},
			{
				// Without the permission, a title edit on a command schedule still
				// applies: the sparse PATCH leaves the action out.
				PreConfig: deny,
				Config:    cmdTitled,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(scheduleAddr, "title", "Hourly"),
					api.wantPatchKeys([]string{"action"}, []string{"action"}, []string{"title"}),
				),
			},
		},
	})
}

// TestBotSchedule_PlanValidation checks the plan-time rules: one timing and
// one action, the enums, a positive interval, a real IANA timezone, an RFC 3339
// anchor, exit-code keys and labels the API would accept unchanged.
func TestBotSchedule_PlanValidation(t *testing.T) {
	api, srv := newFakeScheduleAPI(t)
	cases := map[string]struct{ body, want string }{
		"two timings": {`
  cron   = { cron = "0 9 * * *" }
  rrule  = { rrule = "FREQ=DAILY" }
  prompt = "x"`, `(?s)(Invalid Attribute Combination|Missing Attribute Configuration).*cron.*interval.*rrule`},
		"no timing": {`
  prompt = "x"`, `(?s)(Invalid Attribute Combination|Missing Attribute Configuration).*cron.*interval.*rrule`},
		"no action": {`
  cron = { cron = "0 9 * * *" }`, `(?s)(Invalid Attribute Combination|Missing Attribute Configuration).*prompt.*command`},
		"two actions": {`
  cron    = { cron = "0 9 * * *" }
  prompt  = "x"
  command = { command = "./x" }`, `(?s)(Invalid Attribute Combination|Missing Attribute Configuration).*prompt.*command`},
		"catchup policy": {`
  cron           = { cron = "0 9 * * *" }
  prompt         = "x"
  catchup_policy = "backfill"`, `catchup_policy.*value must be one of`},
		"overlap policy": {`
  cron    = { cron = "0 9 * * *" }
  command = { command = "./x", overlap_policy = "wait" }`, `overlap_policy.*value must be one of`},
		"zero interval": {`
  interval = { interval_seconds = 0 }
  prompt   = "x"`, `interval_seconds.*at least 1`},
		"timezone": {`
  cron   = { cron = "0 9 * * *", timezone = "Mars/Olympus_Mons" }
  prompt = "x"`, `"Mars/Olympus_Mons" is not an IANA timezone`},
		"go-only timezone": {`
  cron   = { cron = "0 9 * * *", timezone = "Local" }
  prompt = "x"`, `"Local" is not an IANA timezone`},
		"naive anchor": {`
  interval = { interval_seconds = 60, anchor = "2026-01-01T09:00:00" }
  prompt   = "x"`, `is not an RFC 3339 timestamp`},
		"sub-microsecond anchor": {`
  interval = { interval_seconds = 60, anchor = "2026-01-01T09:00:00.123456789Z" }
  prompt   = "x"`, `more precise than a microsecond`},
		// Go keeps nine fractional digits and drops the rest, so this parses to
		// exactly .123456: only the digits as written show the extra precision.
		"sub-microsecond past nine digits": {`
  interval = { interval_seconds = 60, anchor = "2026-01-01T09:00:00.1234560001Z" }
  prompt   = "x"`, `more precise than a microsecond`},
		"exit code key": {`
  cron    = { cron = "0 9 * * *" }
  command = { command = "./x", exit_codes = { "07" = "success" } }`, `must be an exit code from`},
		"exit class": {`
  cron    = { cron = "0 9 * * *" }
  command = { command = "./x", exit_codes = { "0" = "ok" } }`, `value must be one of`},
		"untrimmed label": {`
  cron    = { cron = "0 9 * * *" }
  command = { command = "./x", exit_code_labels = { "0" = "Done " } }`, `must not start or end with whitespace`},
		"routed deferred": {`
  cron    = { cron = "0 9 * * *" }
  command = { command = "./x", on_outcome = { deferred = { pause = true } } }`, `value must be one of`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps: []resource.TestStep{{
					Config:      scheduleConfig(srv.URL, tc.body),
					PlanOnly:    true,
					ExpectError: regexp.MustCompile(tc.want),
				}},
			})
		})
	}
	// Every case is refused at plan time, before anything reaches the API.
	if n := len(api.creates) + len(api.patches); n != 0 {
		t.Fatalf("invalid configuration reached the API: %d writes", n)
	}
}

// TestBotSchedule_AnchorPrecision covers fractional anchors, which the API
// echoes in Python isoformat at microsecond precision. A microsecond anchor and
// a 7-digit one with a trailing zero both name an instant the API stores
// exactly: create and update keep the configured spelling, and the harness's
// post-apply plan check proves each re-plans clean.
func TestBotSchedule_AnchorPrecision(t *testing.T) {
	api, srv := newFakeScheduleAPI(t)
	anchored := func(anchor string) string {
		return scheduleConfig(srv.URL, fmt.Sprintf(`
  interval = { interval_seconds = 3600, anchor = %q }
  prompt   = "x"
`, anchor))
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: anchored("2026-01-01T09:00:00.123456Z"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(scheduleAddr, "interval.anchor", "2026-01-01T09:00:00.123456Z"),
					func(*terraform.State) error {
						if got := api.only().specJSON(); got != `{"anchor":"2026-01-01T09:00:00.123456+00:00","interval_seconds":3600}` {
							return fmt.Errorf("stored spec = %s", got)
						}
						return nil
					},
				),
			},
			{
				Config: anchored("2026-01-01T09:00:00.1234570Z"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(scheduleAddr, "interval.anchor", "2026-01-01T09:00:00.1234570Z"),
					api.wantPatchKeys([]string{"schedule"}),
					func(*terraform.State) error {
						if got := api.only().specJSON(); got != `{"anchor":"2026-01-01T09:00:00.123457+00:00","interval_seconds":3600}` {
							return fmt.Errorf("stored spec = %s", got)
						}
						return nil
					},
				),
			},
		},
	})
}

// TestBotSchedule_ImportInvalidID rejects an import ID without both parts.
func TestBotSchedule_ImportInvalidID(t *testing.T) {
	_, srv := newFakeScheduleAPI(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config:        scheduleConfig(srv.URL, "  cron = { cron = \"0 9 * * *\" }\n  prompt = \"x\""),
			ResourceName:  scheduleAddr,
			ImportState:   true,
			ImportStateId: "sch-1",
			ExpectError:   regexp.MustCompile(`Expected <bot_slug>/<schedule_id>`),
		}},
	})
}
