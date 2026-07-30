package brain

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildScriptPromptsIncludesPersonaRulesAndContract(t *testing.T) {
	system, user := BuildScriptPrompts("BẢN SẮC RIÊNG", `{"type":"seam"}`)

	require.Contains(t, system, "BẢN SẮC RIÊNG", "the persona bible leads")
	require.Contains(t, system, SeamRules, "the segment rules must ride in the system prompt")
	require.Contains(t, system, "Output contract")
	require.Contains(t, user, `{"type":"seam"}`)
	require.Contains(t, user, "<brief>", "the brief stays inside its data delimiter")
	require.Less(t, strings.Index(system, SeamRules), strings.Index(system, "Output contract"))
}

// The rules must invite next-track talk, not ban it. The old talkRules said
// "KHÔNG hứa hẹn bài tiếp theo" — that ban is the whole reason she never
// opened a song.
func TestSeamRulesInviteTheIntro(t *testing.T) {
	require.NotContains(t, SeamRules, "KHÔNG hứa hẹn")
	require.Contains(t, SeamRules, "coming_up")
	require.Contains(t, SeamRules, "just_played")
}

// The truth rail: brief facts are plain, model knowledge is hedged.
func TestSeamRulesCarryTheTruthRail(t *testing.T) {
	require.Contains(t, SeamRules, "nhớ không lầm")
}

// Digit-lint is enforced post hoc, but the rules must also SAY it — a retry
// costs a whole extra model call.
func TestSeamRulesForbidNumerals(t *testing.T) {
	require.Contains(t, SeamRules, "viết bằng chữ")
}
