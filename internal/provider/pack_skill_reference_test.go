package provider

import (
	"encoding/json"
	"testing"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

func TestPackSkillReferenceProjection(t *testing.T) {
	for _, tc := range []struct {
		name, body      string
		latest, invalid bool
	}{
		{"fixed", `{"required":true,"skill":{"kind":"fixed","skill_id":"skill-1","skill_slug":"coach","skill_name":"Coach","skill_version_id":"version-1","version_no":2,"version_name":"v2"}}`, false, false},
		{"old API fixed", `{"required":true,"skill":{"skill_id":"skill-1","skill_slug":"coach","skill_name":"Coach","skill_version_id":"version-1","version_no":2,"version_name":"v2"}}`, false, false},
		{"latest", `{"required":true,"skill":{"kind":"latest","skill_id":"skill-1","skill_slug":"coach","skill_name":"Coach"}}`, true, false},
		{"unknown kind", `{"required":true,"skill":{"kind":"future","skill_id":"skill-1"}}`, false, true},
		{"missing fixed version", `{"required":true,"skill":{"kind":"fixed","skill_id":"skill-1"}}`, false, true},
		{"missing latest id", `{"required":true,"skill":{"kind":"latest"}}`, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var s client.NamedPackSkill
			if err := json.Unmarshal([]byte(tc.body), &s); err != nil {
				t.Fatal(err)
			}
			got, err := packSkillToModel(s)
			if tc.invalid {
				if err == nil {
					t.Fatal("unsupported or malformed reference accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.SkillID.ValueString() != "skill-1" || got.SkillSlug.ValueString() != "coach" || got.SkillName.ValueString() != "Coach" || !got.Required.ValueBool() {
				t.Fatalf("identity or required lost: %+v", got)
			}
			if tc.latest {
				if !got.SkillVersionID.IsNull() || !got.VersionNo.IsNull() || !got.VersionName.IsNull() {
					t.Fatalf("latest skill fabricated a pin: %+v", got)
				}
			} else if got.SkillVersionID.ValueString() != "version-1" || got.VersionNo.ValueInt64() != 2 || got.VersionName.ValueString() != "v2" {
				t.Fatalf("fixed version lost: %+v", got)
			}
		})
	}
}
