package provider

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/mapvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/resourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

var (
	_ resource.Resource                     = (*BotScheduleResource)(nil)
	_ resource.ResourceWithConfigure        = (*BotScheduleResource)(nil)
	_ resource.ResourceWithImportState      = (*BotScheduleResource)(nil)
	_ resource.ResourceWithConfigValidators = (*BotScheduleResource)(nil)
)

// BotScheduleResource manages one recurring schedule on a bot (PlatDev #4490).
// It owns only the schedule it created or imported; other schedules on the
// bot, including ones the bot made itself, are never touched. List them with
// the botyard_bot_schedules data source.
type BotScheduleResource struct {
	data *providerData
}

// NewBotScheduleResource is the resource factory registered with the provider.
func NewBotScheduleResource() resource.Resource {
	return &BotScheduleResource{}
}

// Wire values of the schedule enums, as validated at plan time.
var (
	scheduleCatchupPolicies = []string{
		string(client.ScheduleCatchupPolicySkip), string(client.ScheduleCatchupPolicyRunOnceOnRecovery),
	}
	scheduleOverlapPolicies = []string{
		string(client.ScheduleOverlapPolicySkip), string(client.ScheduleOverlapPolicyQueue),
	}
	scheduleNotifyModes = []string{
		string(client.ScheduleNotifyModeOnChange), string(client.ScheduleNotifyModeEveryRun),
	}
	scheduleExitClasses = []string{
		string(client.CommandScheduleActionExitCodesSuccess),
		string(client.CommandScheduleActionExitCodesFailure),
		string(client.CommandScheduleActionExitCodesDeferred),
	}
	// Outcomes on_outcome may route. `deferred` is an outcome too, but it is
	// not final and the API refuses to route it.
	scheduleRoutableOutcomes = []string{
		"success", "failure", "timeout", "lost", "missed", "secret_unavailable",
		"skipped_overlap", "late", "ok", "not_ok",
	}
)

const (
	scheduleDefaultTimezone = "UTC"
	// Shortest window and recheck period the API accepts for a command.
	scheduleCommandMinSeconds = 60
	// Longest label the API keeps for an exit code, after trimming.
	scheduleExitCodeLabelMax = 40
)

// Metadata sets the resource type name.
func (r *BotScheduleResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_bot_schedule"
}

// --- schema -----------------------------------------------------------------

func scheduleTimezoneAttr() schema.StringAttribute {
	return schema.StringAttribute{
		Optional: true,
		Computed: true,
		Default:  stringdefault.StaticString(scheduleDefaultTimezone),
		MarkdownDescription: "IANA timezone the schedule fires in, e.g. `Europe/Copenhagen`. Firing follows " +
			"daylight-saving changes. Defaults to `UTC`.",
		Validators: []validator.String{ianaTimezone()},
	}
}

func exitCodeKeyValidator() validator.String {
	return stringvalidator.RegexMatches(exitCodeKeyPattern, "must be an exit code from `0` to `255`, written without leading zeros, or `*`")
}

func scheduleWakeBotAttr() schema.SingleNestedAttribute {
	return schema.SingleNestedAttribute{
		Optional:            true,
		MarkdownDescription: "Wake the bot with a message.",
		Attributes: map[string]schema.Attribute{
			"message": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Wake message. May use `{outcome}`, `{exit_code}`, `{label}`, `{attempt}`, " +
					"`{log_tail}`, `{output_path}` and `{window_end}`. Write a literal brace as `{{` or `}}`.",
				Validators: []validator.String{stringvalidator.LengthBetween(1, 4000)},
			},
			"include_log_tail": schema.Int64Attribute{
				Optional:            true,
				Computed:            true,
				Default:             int64default.StaticInt64(0),
				MarkdownDescription: "Lines of `log_path` to quote into the wake. `0` (the default) quotes none; anything else needs `log_path`.",
				Validators:          []validator.Int64{int64validator.Between(0, 200)},
			},
		},
	}
}

func scheduleNotifyAttr() schema.SingleNestedAttribute {
	return schema.SingleNestedAttribute{
		Optional:            true,
		MarkdownDescription: "Notify humans.",
		Attributes: map[string]schema.Attribute{
			"recipients": schema.ListAttribute{
				Optional:            true,
				Computed:            true,
				ElementType:         types.StringType,
				Default:             listdefault.StaticValue(types.ListValueMust(types.StringType, []attr.Value{})),
				MarkdownDescription: "User IDs to notify. Empty (the default) notifies the conversation's accountable human.",
				Validators:          []validator.List{listvalidator.SizeAtMost(20), listvalidator.UniqueValues()},
			},
			"mode": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(string(client.ScheduleNotifyModeOnChange)),
				MarkdownDescription: "`on_change` (the default) notifies only when the outcome flips between ok and " +
					"not ok; `every_run` notifies on every matching outcome.",
				Validators: []validator.String{stringvalidator.OneOf(scheduleNotifyModes...)},
			},
		},
	}
}

func schedulePauseAttr() schema.BoolAttribute {
	return schema.BoolAttribute{
		Optional:            true,
		Computed:            true,
		Default:             booldefault.StaticBool(false),
		MarkdownDescription: "Pause the schedule (sets `enabled` to false remotely; the next apply re-enables it).",
	}
}

func scheduleCommandAttr() schema.SingleNestedAttribute {
	return schema.SingleNestedAttribute{
		Optional: true,
		MarkdownDescription: "Run a command on the bot's runtime on each fire. Exactly one of `prompt` or `command`. " +
			"Creating or switching to a command needs the `schedule:schedule_command.create` / `.update` " +
			"permission on top of the schedule permissions.",
		Attributes: map[string]schema.Attribute{
			"command": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Shell command to spawn. Do not redirect its output: the runtime captures " +
					"stdout and stderr into `log_path` itself.",
				Validators: []validator.String{stringvalidator.LengthBetween(1, 8000)},
			},
			"cwd": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Working directory, relative to the workspace root. Omit for the root.",
			},
			"timeout_seconds": schema.Int64Attribute{
				Optional:            true,
				MarkdownDescription: "Deadline per attempt. Omit for no deadline.",
				Validators:          []validator.Int64{int64validator.AtLeast(1)},
			},
			"secret_env": schema.ListNestedAttribute{
				Optional: true,
				Computed: true,
				Default:  listdefault.StaticValue(types.ListValueMust(types.ObjectType{AttrTypes: secretEnvAttrTypes}, []attr.Value{})),
				MarkdownDescription: "Environment variables resolved from the Runtime Vault at each spawn. Values never " +
					"pass through Terraform: each entry names a vault key path.",
				Validators: []validator.List{listvalidator.SizeAtMost(32)},
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"name": schema.StringAttribute{
						Required:            true,
						MarkdownDescription: "Environment variable name. May not use the `BOTYARD_` or `LD_` prefix.",
					},
					"secret_key_path": schema.StringAttribute{
						Required:            true,
						MarkdownDescription: "Runtime Vault key path; never a secret value.",
					},
				}},
			},
			"overridable_env": schema.ListAttribute{
				Optional:    true,
				Computed:    true,
				ElementType: types.StringType,
				Default:     listdefault.StaticValue(types.ListValueMust(types.StringType, []attr.Value{})),
				MarkdownDescription: "Environment variable names a manual trigger may set for one run. Override " +
					"values are stored in run history as plain text, so never use them for secrets.",
				Validators: []validator.List{listvalidator.SizeAtMost(32), listvalidator.UniqueValues()},
			},
			"concurrency_group": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Lock shared across this bot's command schedules. Omit for no lock.",
				Validators:          []validator.String{stringvalidator.LengthBetween(1, 100)},
			},
			"overlap_policy": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(string(client.ScheduleOverlapPolicySkip)),
				MarkdownDescription: "What a fire does while its `concurrency_group` lock is held: `skip` (the " +
					"default) or `queue` (needs a `concurrency_group`).",
				Validators: []validator.String{stringvalidator.OneOf(scheduleOverlapPolicies...)},
			},
			"log_path": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Absolute path the runtime writes the command's stdout and stderr to.",
			},
			"output_path": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Absolute path named in the wake message. The platform never reads it.",
			},
			"exit_codes": schema.MapAttribute{
				Optional:    true,
				Computed:    true,
				ElementType: types.StringType,
				Default: mapdefault.StaticValue(types.MapValueMust(types.StringType, map[string]attr.Value{
					"0": types.StringValue(string(client.CommandScheduleActionExitCodesSuccess)),
					"*": types.StringValue(string(client.CommandScheduleActionExitCodesFailure)),
				})),
				MarkdownDescription: "Exit code (`0`..`255`, or `*` for any other) to `success`, `failure` or " +
					"`deferred`. Defaults to `{\"0\" = \"success\", \"*\" = \"failure\"}`. Declaring the map replaces the " +
					"default entirely. `deferred` needs a `window`.",
				Validators: []validator.Map{
					mapvalidator.SizeAtLeast(1),
					mapvalidator.KeysAre(exitCodeKeyValidator()),
					mapvalidator.ValueStringsAre(stringvalidator.OneOf(scheduleExitClasses...)),
				},
			},
			"exit_code_labels": schema.MapAttribute{
				Optional:    true,
				Computed:    true,
				ElementType: types.StringType,
				Default:     mapdefault.StaticValue(types.MapValueMust(types.StringType, map[string]attr.Value{})),
				MarkdownDescription: "Display label per `exit_codes` key, e.g. `{\"75\" = \"Waiting for input\"}`. " +
					"Single line, 1 to 40 characters, no leading or trailing whitespace.",
				Validators: []validator.Map{
					mapvalidator.KeysAre(exitCodeKeyValidator()),
					mapvalidator.ValueStringsAre(exitCodeLabel()),
				},
			},
			"window": schema.SingleNestedAttribute{
				Optional: true,
				MarkdownDescription: "Run until success within each period: a `deferred` exit is rechecked every " +
					"`recheck_seconds` until success, or until the window closes (outcome `late`).",
				Attributes: map[string]schema.Attribute{
					"duration_seconds": schema.Int64Attribute{
						Required:            true,
						MarkdownDescription: "How long after each period start rechecks may run.",
						Validators:          []validator.Int64{int64validator.Between(scheduleCommandMinSeconds, 7*86400)},
					},
					"recheck_seconds": schema.Int64Attribute{
						Required:            true,
						MarkdownDescription: "Delay between a deferred exit and the next check.",
						Validators:          []validator.Int64{int64validator.AtLeast(scheduleCommandMinSeconds)},
					},
				},
			},
			"on_outcome": schema.MapNestedAttribute{
				Optional: true,
				Computed: true,
				Default:  mapdefault.StaticValue(types.MapValueMust(types.ObjectType{AttrTypes: routingAttrTypes()}, map[string]attr.Value{})),
				MarkdownDescription: "Routing per outcome, keyed by outcome (`success`, `failure`, `timeout`, `lost`, " +
					"`missed`, `secret_unavailable`, `skipped_overlap`, `late`, `ok`, `not_ok`). Empty (the " +
					"default) means success and skipped_overlap do nothing, and every other outcome wakes the bot " +
					"and notifies the accountable human.",
				Validators: []validator.Map{mapvalidator.KeysAre(stringvalidator.OneOf(scheduleRoutableOutcomes...))},
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"retry": schema.SingleNestedAttribute{
						Optional: true,
						MarkdownDescription: "Retry before acting. With `retry`, put the actions under `then`. Only " +
							"failure, timeout, lost, missed and secret_unavailable may retry.",
						Attributes: map[string]schema.Attribute{
							"max": schema.Int64Attribute{
								Required:            true,
								MarkdownDescription: "Retries after the first attempt.",
								Validators:          []validator.Int64{int64validator.Between(1, 10)},
							},
							"backoff_seconds": schema.Int64Attribute{
								Optional:            true,
								Computed:            true,
								Default:             int64default.StaticInt64(0),
								MarkdownDescription: "Delay before each retry. Defaults to `0`.",
								Validators:          []validator.Int64{int64validator.Between(0, 86400)},
							},
						},
					},
					"then": schema.SingleNestedAttribute{
						Optional:            true,
						MarkdownDescription: "Actions applied once retries run out. Only valid with `retry`.",
						Attributes: map[string]schema.Attribute{
							"wake_bot":      scheduleWakeBotAttr(),
							"notify_humans": scheduleNotifyAttr(),
							"pause":         schedulePauseAttr(),
						},
					},
					"wake_bot":      scheduleWakeBotAttr(),
					"notify_humans": scheduleNotifyAttr(),
					"pause":         schedulePauseAttr(),
				}},
			},
		},
	}
}

// Schema defines the botyard_bot_schedule resource schema.
func (r *BotScheduleResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages one recurring schedule on a Botyard bot: when it fires (`cron`, `interval` or " +
			"`rrule`) and what it does (`prompt` wakes the bot, `command` runs a command on its runtime).\n\n" +
			"The resource manages only the schedule it created or imported. Other schedules on the bot, " +
			"including ones the bot created itself, are left alone; list them with the `botyard_bot_schedules` " +
			"data source.\n\n" +
			"**Terraform owns `enabled`.** A schedule paused in the app, or auto-paused after " +
			"`max_consecutive_failures`, shows as drift, and the next apply resumes it. Pause it from " +
			"Terraform by setting `enabled = false`.\n\n" +
			"The next fire time changes on every read, so it is not stored here; read it from " +
			"`botyard_bot_schedules`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The schedule ID.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"bot_slug": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Slug of the bot the schedule belongs to. Changing it forces replacement.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"title": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Human-friendly label. Removing it clears the label.",
			},
			"conversation_id": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Conversation the schedule wakes into; it must be served by the same bot. " +
					"When omitted, Botyard creates a dedicated conversation and this attribute adopts it. " +
					"Changing it moves the schedule's wakes in place.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"enabled": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(true),
				MarkdownDescription: "Whether the schedule fires. Defaults to `true`; a remote pause is planned back.",
			},
			"skip_if_busy": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(false),
				MarkdownDescription: "Skip a fire, rather than count it as missed, when the conversation already has an active turn. Defaults to `false`.",
			},
			"catchup_policy": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(string(client.ScheduleCatchupPolicySkip)),
				MarkdownDescription: "Fires missed while the bot was unavailable: `skip` (the default) drops them; " +
					"`run_once_on_recovery` fires once when the bot is reachable again.",
				Validators: []validator.String{stringvalidator.OneOf(scheduleCatchupPolicies...)},
			},
			"max_consecutive_failures": schema.Int64Attribute{
				Optional:            true,
				Computed:            true,
				Default:             int64default.StaticInt64(0),
				MarkdownDescription: "Auto-pause after this many consecutive failures. `0` (the default) never auto-pauses.",
				Validators:          []validator.Int64{int64validator.AtLeast(0)},
			},
			"cron": schema.SingleNestedAttribute{
				Optional:            true,
				MarkdownDescription: "Fire on a cron expression. Exactly one of `cron`, `interval` or `rrule`; switching kind updates in place.",
				Attributes: map[string]schema.Attribute{
					"cron": schema.StringAttribute{
						Required:            true,
						MarkdownDescription: "Cron expression, e.g. `0 9 * * 1-5`.",
						Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
					},
					"timezone": scheduleTimezoneAttr(),
				},
			},
			"interval": schema.SingleNestedAttribute{
				Optional:            true,
				MarkdownDescription: "Fire on a fixed period. Exactly one of `cron`, `interval` or `rrule`.",
				Attributes: map[string]schema.Attribute{
					"interval_seconds": schema.Int64Attribute{
						Required:            true,
						MarkdownDescription: "Period between fires, in seconds.",
						Validators:          []validator.Int64{int64validator.AtLeast(1)},
					},
					"anchor": schema.StringAttribute{
						Optional: true,
						MarkdownDescription: "RFC 3339 instant the period is aligned to, e.g. `2026-01-01T09:00:00Z`. " +
							"Omit to align to the Unix epoch.",
						Validators: []validator.String{rfc3339Instant()},
					},
					"timezone": scheduleTimezoneAttr(),
				},
			},
			"rrule": schema.SingleNestedAttribute{
				Optional:            true,
				MarkdownDescription: "Fire on an RFC 5545 recurrence rule. Exactly one of `cron`, `interval` or `rrule`.",
				Attributes: map[string]schema.Attribute{
					"rrule": schema.StringAttribute{
						Required:            true,
						MarkdownDescription: "Recurring RFC 5545 rule with at least two occurrences, e.g. `FREQ=WEEKLY;INTERVAL=2;BYDAY=MO`. May embed `DTSTART`.",
						Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
					},
					"timezone": scheduleTimezoneAttr(),
				},
			},
			"prompt": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Wake message delivered to the bot on each fire. Exactly one of `prompt` or `command`.",
				Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"command": scheduleCommandAttr(),
		},
	}
}

// ConfigValidators enforces one variant per union.
func (r *BotScheduleResource) ConfigValidators(context.Context) []resource.ConfigValidator {
	return []resource.ConfigValidator{
		resourcevalidator.ExactlyOneOf(path.MatchRoot("cron"), path.MatchRoot("interval"), path.MatchRoot("rrule")),
		resourcevalidator.ExactlyOneOf(path.MatchRoot("prompt"), path.MatchRoot("command")),
	}
}

// Attribute types of the nested objects that need a typed empty default.
var secretEnvAttrTypes = map[string]attr.Type{"name": types.StringType, "secret_key_path": types.StringType}

func routingAttrTypes() map[string]attr.Type {
	wake := types.ObjectType{AttrTypes: map[string]attr.Type{"message": types.StringType, "include_log_tail": types.Int64Type}}
	notify := types.ObjectType{AttrTypes: map[string]attr.Type{"recipients": types.ListType{ElemType: types.StringType}, "mode": types.StringType}}
	return map[string]attr.Type{
		"retry": types.ObjectType{AttrTypes: map[string]attr.Type{"max": types.Int64Type, "backoff_seconds": types.Int64Type}},
		"then": types.ObjectType{AttrTypes: map[string]attr.Type{
			"wake_bot": wake, "notify_humans": notify, "pause": types.BoolType,
		}},
		"wake_bot":      wake,
		"notify_humans": notify,
		"pause":         types.BoolType,
	}
}

// Configure receives the shared provider data.
func (r *BotScheduleResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	data, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError("Unexpected Resource Configure Type",
			fmt.Sprintf("Expected *providerData, got: %T. This is a bug in the provider.", req.ProviderData))
		return
	}
	r.data = data
}

// --- CRUD -------------------------------------------------------------------

func (r *BotScheduleResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan botScheduleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	spec, d := decodeScheduleSpec(&plan)
	resp.Diagnostics.Append(d...)
	action, d := decodeScheduleAction(&plan)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	body, err := client.NewScheduleCreate(spec, action)
	if err != nil {
		resp.Diagnostics.AddError("Error encoding schedule", err.Error())
		return
	}
	body.Title = plan.Title.ValueStringPointer()
	if !plan.ConversationID.IsUnknown() {
		body.ConversationId = plan.ConversationID.ValueStringPointer()
	}
	body.Enabled = plan.Enabled.ValueBoolPointer()
	body.SkipIfBusy = plan.SkipIfBusy.ValueBoolPointer()
	catchup := client.ScheduleCatchupPolicy(plan.CatchupPolicy.ValueString())
	body.CatchupPolicy = &catchup
	body.MaxConsecutiveFailures = int64ToIntPtr(plan.MaxConsecutiveFailures)

	slug := plan.BotSlug.ValueString()
	out, err := r.data.client.CreateScheduleV1OrgsOrgIdBotsBotSlugSchedulesPostWithResponse(ctx, r.data.orgID, slug, body)
	if err != nil {
		resp.Diagnostics.AddError("Error creating bot schedule", err.Error())
		return
	}
	if out.JSON201 == nil {
		scheduleWriteError(&resp.Diagnostics, "create", out.StatusCode(), out.Body, action)
		return
	}
	resp.Diagnostics.Append(plan.setFromResponse(*out.JSON201, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *BotScheduleResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state botScheduleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.data.client.GetScheduleV1OrgsOrgIdBotsBotSlugSchedulesScheduleIdGetWithResponse(
		ctx, r.data.orgID, state.BotSlug.ValueString(), state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Error reading bot schedule", err.Error())
		return
	}
	if out.StatusCode() == http.StatusNotFound {
		// The schedule, or its bot, is gone: drop it so the next plan recreates it.
		resp.State.RemoveResource(ctx)
		return
	}
	if out.JSON200 == nil {
		resp.Diagnostics.AddError("Unexpected response reading bot schedule",
			fmt.Sprintf("Get returned HTTP %d: %s", out.StatusCode(), describeAPIError(out.Body)))
		return
	}
	prior := state
	resp.Diagnostics.Append(state.setFromResponse(*out.JSON200, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update sends a sparse PATCH holding only what the plan changed. The state
// the framework hands Update is the prior state, so the PATCH is built from
// the plan-vs-state difference and the state is rewritten from the response.
func (r *BotScheduleResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state botScheduleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	patch, sendsAction, d := buildSchedulePatch(&plan, &state)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}

	slug, id := plan.BotSlug.ValueString(), state.ID.ValueString()
	var got *client.ScheduleResponse
	if patch.IsEmpty() {
		out, err := r.data.client.GetScheduleV1OrgsOrgIdBotsBotSlugSchedulesScheduleIdGetWithResponse(ctx, r.data.orgID, slug, id)
		if err != nil {
			resp.Diagnostics.AddError("Error reading bot schedule", err.Error())
			return
		}
		if out.JSON200 == nil {
			resp.Diagnostics.AddError("Unexpected response reading bot schedule",
				fmt.Sprintf("Get returned HTTP %d: %s", out.StatusCode(), describeAPIError(out.Body)))
			return
		}
		got = out.JSON200
	} else {
		out, err := r.data.client.PatchSchedule(ctx, r.data.orgID, slug, id, patch)
		if err != nil {
			resp.Diagnostics.AddError("Error updating bot schedule", err.Error())
			return
		}
		if out.JSON200 == nil {
			scheduleWriteError(&resp.Diagnostics, "update", out.StatusCode(), out.Body, sendsAction)
			return
		}
		got = out.JSON200
	}
	resp.Diagnostics.Append(plan.setFromResponse(*got, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// buildSchedulePatch returns the PATCH for plan-vs-state, plus the action it
// sends (nil when the action is unchanged), for the permission diagnostic.
func buildSchedulePatch(plan, state *botScheduleModel) (client.SchedulePatch, client.ScheduleAction, diag.Diagnostics) {
	var p client.SchedulePatch
	var diags diag.Diagnostics
	if !plan.Title.Equal(state.Title) {
		p.Title = &client.NullableString{Value: plan.Title.ValueStringPointer()}
	}
	if !plan.ConversationID.IsUnknown() && !plan.ConversationID.IsNull() && !plan.ConversationID.Equal(state.ConversationID) {
		p.ConversationID = plan.ConversationID.ValueStringPointer()
	}
	if !plan.Enabled.Equal(state.Enabled) {
		p.Enabled = plan.Enabled.ValueBoolPointer()
	}
	if !plan.SkipIfBusy.Equal(state.SkipIfBusy) {
		p.SkipIfBusy = plan.SkipIfBusy.ValueBoolPointer()
	}
	if !plan.CatchupPolicy.Equal(state.CatchupPolicy) {
		c := client.ScheduleCatchupPolicy(plan.CatchupPolicy.ValueString())
		p.CatchupPolicy = &c
	}
	if !plan.MaxConsecutiveFailures.Equal(state.MaxConsecutiveFailures) {
		p.MaxConsecutiveFailures = int64ToIntPtr(plan.MaxConsecutiveFailures)
	}

	planSpec, d := decodeScheduleSpec(plan)
	diags.Append(d...)
	stateSpec, d := decodeScheduleSpec(state)
	diags.Append(d...)
	planAction, d := decodeScheduleAction(plan)
	diags.Append(d...)
	stateAction, d := decodeScheduleAction(state)
	diags.Append(d...)
	if diags.HasError() {
		return p, nil, diags
	}
	if same, err := client.SameScheduleSpec(planSpec, stateSpec); err != nil {
		diags.AddError("Error encoding schedule", err.Error())
	} else if !same {
		if err := p.SetSchedule(planSpec); err != nil {
			diags.AddError("Error encoding schedule", err.Error())
		}
	}
	var sent client.ScheduleAction
	if same, err := client.SameScheduleAction(planAction, stateAction); err != nil {
		diags.AddError("Error encoding schedule action", err.Error())
	} else if !same {
		if err := p.SetAction(planAction); err != nil {
			diags.AddError("Error encoding schedule action", err.Error())
		}
		sent = planAction
	}
	return p, sent, diags
}

func (r *BotScheduleResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state botScheduleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	status, body, err := r.data.client.DeleteSchedule(ctx, r.data.orgID, state.BotSlug.ValueString(), state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Error deleting bot schedule", err.Error())
		return
	}
	switch status {
	case http.StatusNoContent, http.StatusOK, http.StatusNotFound:
		// deleted, or already gone
	default:
		resp.Diagnostics.AddError("Unexpected response deleting bot schedule",
			fmt.Sprintf("Delete returned HTTP %d: %s", status, describeAPIError(body)))
	}
}

// ImportState accepts `<bot_slug>/<schedule_id>`.
func (r *BotScheduleResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	slug, id, ok := strings.Cut(req.ID, "/")
	if !ok || slug == "" || id == "" || strings.Contains(id, "/") {
		resp.Diagnostics.AddError("Invalid import ID",
			fmt.Sprintf("Expected <bot_slug>/<schedule_id>, got %q.", req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("bot_slug"), slug)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), id)...)
}

// scheduleWriteError reports a failed create/update. A 403 while a command
// action is being written names the command permission, which a role can lack
// while holding every schedule permission.
func scheduleWriteError(diags *diag.Diagnostics, op string, status int, body []byte, action client.ScheduleAction) {
	_, isCommand := action.(*client.ScheduleActionCommand)
	if status == http.StatusForbidden && isCommand && bytes.Contains(body, []byte("schedule_command")) {
		diags.AddError("Missing permission for a command schedule",
			fmt.Sprintf("Botyard refused to %[1]s a command schedule: the API key's role lacks the "+
				"`schedule:schedule_command.%[1]s` permission, which command schedules need on top of the "+
				"schedule permissions a prompt schedule uses. Grant it to the key's role, or use `prompt` "+
				"instead of `command`. API response: %[2]s", op, describeAPIError(body)))
		return
	}
	diags.AddError(fmt.Sprintf("Unexpected response trying to %s bot schedule", op),
		fmt.Sprintf("HTTP %d: %s", status, describeAPIError(body)))
}
