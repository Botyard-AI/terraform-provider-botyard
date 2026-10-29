package provider

import (
	"context"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

// `suggestions_opt_out` on botyard_bot (PlatDev #4028a, decision D1 #4374).
//
// The bot has ONE flag covering both skill and tool suggestions
// (`bots.suggestions_opt_out`, NOT NULL, default false). It is not on
// BotResponse: it is read from `GET /bots/{slug}/suggestions` (`.opt_out`) and
// written with `PUT /bots/{slug}/suggestions/opt-out {"opt_out": bool}`, whose
// response is the same SuggestionsResponse. Both routes are gated by
// `require_bot_owner`: an API key passes only on bots it created (D6 #4379,
// keep_403).
//
// Optional+Computed with the stock UseStateForUnknown. The attribute is a root
// leaf, so the nested-parent problem nestedUseStateForUnknown solves does not
// arise: on create there is no prior state (it stays unknown when omitted), and
// on update the prior value is always a real refreshed one. Import leaves it
// null until the first Read fills it from the GET, so an imported bot re-plans
// clean whether or not the configuration declares it.

const suggestionsOptOutAttr = "suggestions_opt_out"

func suggestionsOptOutSchemaAttribute() schema.BoolAttribute {
	return schema.BoolAttribute{
		Optional: true,
		Computed: true,
		MarkdownDescription: "Opt the bot out of skill **and** tool suggestions. One platform flag covers both. " +
			"`true` hides the suggestions banner for this bot; `false` turns suggestions back on. " +
			"When omitted, Terraform reads the current value and leaves it alone.\n\n" +
			"Reading and changing it require bot ownership, and an API key owns only the bots it created. On " +
			"a bot created in the Botyard app or with another key, Terraform cannot read the flag: it keeps the " +
			"last value it knew (none after an import), so a refresh or plan is unaffected, and changing it " +
			"fails with a \"Not the bot's owner\" error.",
		PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
	}
}

// suggestionsOptOutRead is the outcome of reading the flag.
type suggestionsOptOutRead int

const (
	suggestionsOptOutReadOK suggestionsOptOutRead = iota
	// suggestionsOptOutReadForbidden: the caller does not own the bot, so the
	// flag is unreadable. Not an error for Read, so refresh and import of a
	// bot the key does not own keep working; the attribute keeps its prior
	// state value. (Every write to such a bot is refused server-side anyway.)
	suggestionsOptOutReadForbidden
	suggestionsOptOutReadFailed
)

// readSuggestionsOptOut GETs the bot's suggestions surface and returns its
// opt_out flag. A 403 (not the bot's owner) is reported as Forbidden without a
// diagnostic; anything else that is not a 200 adds an error.
func (r *BotResource) readSuggestionsOptOut(ctx context.Context, slug string, diags *diag.Diagnostics) (types.Bool, suggestionsOptOutRead) {
	apiResp, err := r.data.client.GetSuggestionsV1OrgsOrgIdBotsBotSlugSuggestionsGetWithResponse(ctx, r.data.orgID, slug)
	if err != nil {
		diags.AddAttributeError(path.Root(suggestionsOptOutAttr), "Error reading bot suggestions opt-out", err.Error())
		return types.BoolNull(), suggestionsOptOutReadFailed
	}
	switch {
	case apiResp.JSON200 != nil:
		return types.BoolValue(apiResp.JSON200.OptOut), suggestionsOptOutReadOK
	case apiResp.StatusCode() == http.StatusForbidden:
		tflog.Debug(ctx, "suggestions_opt_out unreadable: caller does not own the bot; keeping the prior value",
			map[string]any{"bot_slug": slug})
		return types.BoolNull(), suggestionsOptOutReadForbidden
	default:
		diags.AddAttributeError(path.Root(suggestionsOptOutAttr), "Unexpected response reading bot suggestions opt-out",
			fmt.Sprintf("GET suggestions returned HTTP %d: %s", apiResp.StatusCode(), describeAPIError(apiResp.Body)))
		return types.BoolNull(), suggestionsOptOutReadFailed
	}
}

// writeSuggestionsOptOut PUTs the flag and returns the value the server reports
// back. ok is false when a diagnostic was added.
func (r *BotResource) writeSuggestionsOptOut(ctx context.Context, slug string, optOut bool, diags *diag.Diagnostics) (types.Bool, bool) {
	apiResp, err := r.data.client.SetOptOutV1OrgsOrgIdBotsBotSlugSuggestionsOptOutPutWithResponse(
		ctx, r.data.orgID, slug, client.SuggestionsOptOutRequest{OptOut: optOut})
	if err != nil {
		diags.AddAttributeError(path.Root(suggestionsOptOutAttr), "Error setting bot suggestions opt-out", err.Error())
		return types.BoolNull(), false
	}
	if apiResp.JSON200 != nil {
		return types.BoolValue(apiResp.JSON200.OptOut), true
	}
	if apiResp.StatusCode() == http.StatusForbidden {
		addBotOwnerForbiddenError(diags, path.Root(suggestionsOptOutAttr), slug, apiResp.Body)
		return types.BoolNull(), false
	}
	diags.AddAttributeError(path.Root(suggestionsOptOutAttr), "Unexpected response setting bot suggestions opt-out",
		fmt.Sprintf("PUT suggestions/opt-out returned HTTP %d: %s", apiResp.StatusCode(), describeAPIError(apiResp.Body)))
	return types.BoolNull(), false
}

// addBotOwnerForbiddenError is the D6 (#4379, keep_403) diagnostic for an
// owner-gated bot write that came back 403: it names the setting and explains
// that the provider's API key does not own the bot.
func addBotOwnerForbiddenError(diags *diag.Diagnostics, attr path.Path, slug string, body []byte) {
	diags.AddAttributeError(attr, "Not the bot's owner",
		fmt.Sprintf("Botyard refused to change %s on bot %q (HTTP 403: %s).\n\n"+
			"Only a bot owner can change this setting. An API key owns only the bots it created itself, "+
			"so it cannot change bots created in the Botyard app or with another key, even if the key's "+
			"organization role is owner. Change the setting in the Botyard app, remove it from this "+
			"configuration, or manage the bot with the key that created it.",
			attr, slug, describeAPIError(body)))
}

// applySuggestionsOptOutOnCreate settles the attribute after the bot exists.
// A configured value is written (explicit false included, so the declared value
// is what the server holds, not an assumed default); an omitted one is read.
func (r *BotResource) applySuggestionsOptOutOnCreate(ctx context.Context, slug string, planned types.Bool, diags *diag.Diagnostics) types.Bool {
	if !planned.IsUnknown() && !planned.IsNull() {
		got, _ := r.writeSuggestionsOptOut(ctx, slug, planned.ValueBool(), diags)
		return got
	}
	got, _ := r.readSuggestionsOptOut(ctx, slug, diags)
	return got
}

// applySuggestionsOptOutOnUpdate writes the flag only when the plan changes it.
// An unchanged value is carried through without a request.
func (r *BotResource) applySuggestionsOptOutOnUpdate(ctx context.Context, slug string, planned, prior types.Bool, diags *diag.Diagnostics) (types.Bool, bool) {
	switch {
	case planned.IsUnknown():
		got, outcome := r.readSuggestionsOptOut(ctx, slug, diags)
		return got, outcome != suggestionsOptOutReadFailed
	case planned.IsNull() || planned.Equal(prior):
		return planned, true
	default:
		return r.writeSuggestionsOptOut(ctx, slug, planned.ValueBool(), diags)
	}
}

// refreshSuggestionsOptOut updates the attribute in Read. Forbidden keeps the
// prior value; a failure has already added an error.
func (r *BotResource) refreshSuggestionsOptOut(ctx context.Context, slug string, prior types.Bool, diags *diag.Diagnostics) types.Bool {
	got, outcome := r.readSuggestionsOptOut(ctx, slug, diags)
	if outcome == suggestionsOptOutReadOK {
		return got
	}
	return prior
}
