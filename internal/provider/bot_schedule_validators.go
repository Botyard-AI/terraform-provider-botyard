package provider

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	// Embeds the IANA database so timezone validation does not depend on the
	// host running Terraform having tzdata installed.
	_ "time/tzdata"

	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
)

// exitCodeKeyPattern matches the API's exit_codes keys: a decimal 0..255 with
// no leading zero (the API compares str(int(key)) == key), or `*`.
var exitCodeKeyPattern = regexp.MustCompile(`^(\*|[0-9]|[1-9][0-9]|1[0-9]{2}|2[0-4][0-9]|25[0-5])$`)

// stringCheck adapts a plain check function to validator.String. The check
// returns "" for a valid value, or the reason it is invalid.
type stringCheck struct {
	desc  string
	check func(string) string
}

func (v stringCheck) Description(context.Context) string             { return v.desc }
func (v stringCheck) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (v stringCheck) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if reason := v.check(req.ConfigValue.ValueString()); reason != "" {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid value", reason)
	}
}

// ianaTimezone checks the name against the IANA database embedded in the
// provider, which is the same database the API resolves it against.
func ianaTimezone() validator.String {
	return stringCheck{
		desc: "value must be an IANA timezone name, e.g. `UTC` or `Europe/Copenhagen`",
		check: func(s string) string {
			// LoadLocation accepts "" and "Local" as Go-specific aliases the API does not.
			if s == "" || s == "Local" {
				return fmt.Sprintf("%q is not an IANA timezone name, e.g. \"UTC\" or \"Europe/Copenhagen\".", s)
			}
			if _, err := time.LoadLocation(s); err != nil {
				return fmt.Sprintf("%q is not an IANA timezone name, e.g. \"UTC\" or \"Europe/Copenhagen\".", s)
			}
			return ""
		},
	}
}

// rfc3339Instant requires an RFC 3339 timestamp with an offset, so the anchor
// names one instant rather than a wall-clock time.
func rfc3339Instant() validator.String {
	return stringCheck{
		desc: "value must be an RFC 3339 timestamp with an offset, e.g. `2026-01-01T09:00:00Z`",
		check: func(s string) string {
			if _, err := time.Parse(time.RFC3339, s); err != nil {
				return fmt.Sprintf("%q is not an RFC 3339 timestamp with an offset, e.g. \"2026-01-01T09:00:00Z\".", s)
			}
			return ""
		},
	}
}

// exitCodeLabel mirrors the API's label rule. The API trims each label before
// storing it, so a label with surrounding whitespace would read back different
// from configuration and diff forever: it is refused here instead.
func exitCodeLabel() validator.String {
	return stringCheck{
		desc: fmt.Sprintf("value must be a single line of 1 to %d characters with no leading or trailing whitespace", scheduleExitCodeLabelMax),
		check: func(s string) string {
			switch {
			case strings.ContainsAny(s, "\r\n"):
				return "An exit code label must be a single line."
			case strings.TrimSpace(s) != s:
				return "An exit code label must not start or end with whitespace: the API trims it, which would show as a permanent diff."
			case s == "" || len([]rune(s)) > scheduleExitCodeLabelMax:
				return fmt.Sprintf("An exit code label must be 1 to %d characters.", scheduleExitCodeLabelMax)
			}
			return ""
		},
	}
}
