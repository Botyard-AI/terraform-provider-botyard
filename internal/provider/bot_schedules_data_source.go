package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

var (
	_ datasource.DataSource              = (*BotSchedulesDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*BotSchedulesDataSource)(nil)
)

// BotSchedulesDataSource lists every recurring schedule on a bot, including
// ones the bot created itself (PlatDev #4490). It is read-only discovery: find
// a schedule's ID to import it into botyard_bot_schedule, or read its next
// fire time, which the resource does not store.
type BotSchedulesDataSource struct {
	data *providerData
}

// NewBotSchedulesDataSource is the data source factory registered with the provider.
func NewBotSchedulesDataSource() datasource.DataSource {
	return &BotSchedulesDataSource{}
}

type botSchedulesDataSourceModel struct {
	BotSlug   types.String              `tfsdk:"bot_slug"`
	Schedules []botScheduleSummaryModel `tfsdk:"schedules"`
}

// botScheduleSummaryModel is one listed schedule. Timing and action are unions
// here too: one per-kind object is set and the others are null.
type botScheduleSummaryModel struct {
	ID                     types.String                  `tfsdk:"id"`
	Title                  types.String                  `tfsdk:"title"`
	ConversationID         types.String                  `tfsdk:"conversation_id"`
	Enabled                types.Bool                    `tfsdk:"enabled"`
	SkipIfBusy             types.Bool                    `tfsdk:"skip_if_busy"`
	CatchupPolicy          types.String                  `tfsdk:"catchup_policy"`
	MaxConsecutiveFailures types.Int64                   `tfsdk:"max_consecutive_failures"`
	SpecKind               types.String                  `tfsdk:"spec_kind"`
	Timezone               types.String                  `tfsdk:"timezone"`
	Cron                   *scheduleSummaryCronModel     `tfsdk:"cron"`
	Interval               *scheduleSummaryIntervalModel `tfsdk:"interval"`
	Rrule                  *scheduleSummaryRruleModel    `tfsdk:"rrule"`
	ActionKind             types.String                  `tfsdk:"action_kind"`
	Prompt                 types.String                  `tfsdk:"prompt"`
	Command                *scheduleSummaryCommandModel  `tfsdk:"command"`
	NextFireAt             types.String                  `tfsdk:"next_fire_at"`
	CreatedAt              types.String                  `tfsdk:"created_at"`
	UpdatedAt              types.String                  `tfsdk:"updated_at"`
}

type scheduleSummaryCronModel struct {
	Cron types.String `tfsdk:"cron"`
}

type scheduleSummaryIntervalModel struct {
	IntervalSeconds types.Int64  `tfsdk:"interval_seconds"`
	Anchor          types.String `tfsdk:"anchor"`
}

type scheduleSummaryRruleModel struct {
	Rrule types.String `tfsdk:"rrule"`
}

// scheduleSummaryCommandModel is the command's identity. Its run policy (exit
// codes, window, routing, env) is on botyard_bot_schedule after an import.
type scheduleSummaryCommandModel struct {
	Command          types.String `tfsdk:"command"`
	Cwd              types.String `tfsdk:"cwd"`
	TimeoutSeconds   types.Int64  `tfsdk:"timeout_seconds"`
	ConcurrencyGroup types.String `tfsdk:"concurrency_group"`
	OverlapPolicy    types.String `tfsdk:"overlap_policy"`
	LogPath          types.String `tfsdk:"log_path"`
	OutputPath       types.String `tfsdk:"output_path"`
}

func (d *BotSchedulesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_bot_schedules"
}

func (d *BotSchedulesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	str := func(desc string) schema.StringAttribute {
		return schema.StringAttribute{Computed: true, MarkdownDescription: desc}
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists every recurring schedule on a Botyard bot, including schedules the bot created " +
			"itself. Use it to find a schedule's ID for `terraform import botyard_bot_schedule.<name> " +
			"<bot_slug>/<id>`, or to read `next_fire_at`, which `botyard_bot_schedule` does not store.",
		Attributes: map[string]schema.Attribute{
			"bot_slug": schema.StringAttribute{Required: true, MarkdownDescription: "Slug of the bot whose schedules to list."},
			"schedules": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "The bot's recurring schedules, in the order the API returns them.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"id":                       str("The schedule ID."),
					"title":                    str("Human-friendly label, or null."),
					"conversation_id":          str("Conversation the schedule wakes into."),
					"enabled":                  schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the schedule fires. False when paused, including an auto-pause."},
					"skip_if_busy":             schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether a fire is skipped while the conversation is busy."},
					"catchup_policy":           str("`skip` or `run_once_on_recovery`."),
					"max_consecutive_failures": schema.Int64Attribute{Computed: true, MarkdownDescription: "Auto-pause threshold; `0` never auto-pauses."},
					"spec_kind": str("Timing kind: `cron`, `interval` or `rrule`. A kind this provider version does not " +
						"know (such as `event`) is reported here, with `cron`, `interval` and `rrule` all null."),
					"timezone": str("IANA timezone the schedule fires in."),
					"cron": schema.SingleNestedAttribute{
						Computed: true, MarkdownDescription: "Set when `spec_kind` is `cron`.",
						Attributes: map[string]schema.Attribute{"cron": str("Cron expression.")},
					},
					"interval": schema.SingleNestedAttribute{
						Computed: true, MarkdownDescription: "Set when `spec_kind` is `interval`.",
						Attributes: map[string]schema.Attribute{
							"interval_seconds": schema.Int64Attribute{Computed: true, MarkdownDescription: "Period between fires, in seconds."},
							"anchor":           str("RFC 3339 phase anchor, or null for the Unix epoch."),
						},
					},
					"rrule": schema.SingleNestedAttribute{
						Computed: true, MarkdownDescription: "Set when `spec_kind` is `rrule`.",
						Attributes: map[string]schema.Attribute{"rrule": str("RFC 5545 recurrence rule.")},
					},
					"action_kind": str("`prompt` or `command`."),
					"prompt":      str("Wake message, when `action_kind` is `prompt`."),
					"command": schema.SingleNestedAttribute{
						Computed: true,
						MarkdownDescription: "Set when `action_kind` is `command`. The command's run policy (exit codes, " +
							"window, routing, env) is not listed here; import the schedule to manage it.",
						Attributes: map[string]schema.Attribute{
							"command":           str("Shell command."),
							"cwd":               str("Working directory relative to the workspace root, or null."),
							"timeout_seconds":   schema.Int64Attribute{Computed: true, MarkdownDescription: "Deadline per attempt, or null."},
							"concurrency_group": str("Lock shared across the bot's command schedules, or null."),
							"overlap_policy":    str("`skip` or `queue`."),
							"log_path":          str("Absolute log path, or null."),
							"output_path":       str("Absolute output path, or null."),
						},
					},
					"next_fire_at": str("Next occurrence (RFC 3339), derived on every read. Null when paused or when no occurrence remains."),
					"created_at":   str("Creation time (RFC 3339)."),
					"updated_at":   str("Last update time (RFC 3339)."),
				}},
			},
		},
	}
}

func (d *BotSchedulesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if data := configureDataSourceProviderData(req, resp); data != nil {
		d.data = data
	}
}

// Read lists the bot's schedules. The list endpoint returns them all in one
// response; it does not page.
func (d *BotSchedulesDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var m botSchedulesDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	slug := m.BotSlug.ValueString()
	out, err := d.data.client.ListSchedulesV1OrgsOrgIdBotsBotSlugSchedulesGetWithResponse(ctx, d.data.orgID, slug)
	if err != nil {
		resp.Diagnostics.AddError("Error listing bot schedules", err.Error())
		return
	}
	if out.JSON200 == nil {
		summary := "Unexpected response listing bot schedules"
		if out.StatusCode() == http.StatusNotFound {
			summary = fmt.Sprintf("Bot %q not found", slug)
		}
		resp.Diagnostics.AddError(summary, fmt.Sprintf("List returned HTTP %d: %s", out.StatusCode(), describeAPIError(out.Body)))
		return
	}
	m.Schedules = make([]botScheduleSummaryModel, 0, len(out.JSON200.Schedules))
	for _, s := range out.JSON200.Schedules {
		summary, diags := scheduleSummary(s)
		resp.Diagnostics.Append(diags...)
		if resp.Diagnostics.HasError() {
			return
		}
		m.Schedules = append(m.Schedules, summary)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func scheduleSummary(s client.ScheduleResponse) (botScheduleSummaryModel, diag.Diagnostics) {
	var diags diag.Diagnostics
	out := botScheduleSummaryModel{
		ID:                     types.StringValue(s.ScheduleId),
		Title:                  types.StringPointerValue(s.Title),
		ConversationID:         types.StringValue(s.ConversationId),
		Enabled:                types.BoolValue(s.Enabled),
		SkipIfBusy:             types.BoolValue(s.SkipIfBusy),
		CatchupPolicy:          types.StringValue(string(s.CatchupPolicy)),
		MaxConsecutiveFailures: types.Int64Value(int64(s.MaxConsecutiveFailures)),
		SpecKind:               types.StringValue(string(s.SpecKind)),
		Timezone:               types.StringValue(s.Timezone),
		Prompt:                 types.StringNull(),
		NextFireAt:             timeValue(s.NextFireAt),
		CreatedAt:              types.StringValue(s.CreatedAt.Format(time.RFC3339Nano)),
		UpdatedAt:              types.StringValue(s.UpdatedAt.Format(time.RFC3339Nano)),
	}

	// A timing kind this provider does not know stays listed, with only
	// spec_kind set: the list is discovery, and hiding a schedule is worse.
	spec, err := client.DecodeScheduleSpec(s)
	var unknown *client.UnknownDiscriminatorError
	switch {
	case errors.As(err, &unknown):
	case err != nil:
		diags.AddError("Error reading schedule timing", fmt.Sprintf("Schedule %s: %s", s.ScheduleId, err))
		return out, diags
	default:
		switch v := spec.(type) {
		case *client.ScheduleSpecCron:
			out.Cron = &scheduleSummaryCronModel{Cron: types.StringValue(v.Spec.Cron)}
		case *client.ScheduleSpecInterval:
			out.Interval = &scheduleSummaryIntervalModel{
				IntervalSeconds: types.Int64Value(int64(v.Spec.IntervalSeconds)),
				Anchor:          timeValue(v.Spec.Anchor),
			}
		case *client.ScheduleSpecRrule:
			out.Rrule = &scheduleSummaryRruleModel{Rrule: types.StringValue(v.Spec.Rrule)}
		default:
			diags.AddError("Error reading schedule timing", fmt.Sprintf("Unhandled timing variant %T. This is a bug in the provider.", spec))
			return out, diags
		}
	}

	action, err := client.DecodeScheduleAction(s.Action)
	if err != nil {
		diags.AddError("Error reading schedule action", fmt.Sprintf("Schedule %s: %s", s.ScheduleId, err))
		return out, diags
	}
	out.ActionKind = types.StringValue(action.Kind())
	switch a := action.(type) {
	case *client.ScheduleActionPrompt:
		out.Prompt = types.StringValue(a.Action.Prompt)
	case *client.ScheduleActionCommand:
		c := a.Action
		overlap := types.StringValue(string(client.ScheduleOverlapPolicySkip))
		if c.OverlapPolicy != nil {
			overlap = types.StringValue(string(*c.OverlapPolicy))
		}
		out.Command = &scheduleSummaryCommandModel{
			Command:          types.StringValue(c.Command),
			Cwd:              types.StringPointerValue(c.Cwd),
			TimeoutSeconds:   intPtrToInt64(c.TimeoutSeconds),
			ConcurrencyGroup: types.StringPointerValue(c.ConcurrencyGroup),
			OverlapPolicy:    overlap,
			LogPath:          types.StringPointerValue(c.LogPath),
			OutputPath:       types.StringPointerValue(c.OutputPath),
		}
	default:
		diags.AddError("Error reading schedule action", fmt.Sprintf("Unhandled action variant %T. This is a bug in the provider.", action))
	}
	return out, diags
}

func timeValue(t *time.Time) types.String {
	if t == nil {
		return types.StringNull()
	}
	return types.StringValue(t.Format(time.RFC3339Nano))
}
