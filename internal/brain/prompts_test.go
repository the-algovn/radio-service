package brain

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildScriptPromptsIncludesPersonaRulesAndContract(t *testing.T) {
	system, user := BuildScriptPrompts("BẢN SẮC RIÊNG", SeamRules, `{"type":"seam"}`)

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

// generatedKinds is every kind the brain writes a script for. A station ID is
// absent on purpose: it reads a pre-written line and makes no model call, so
// it has no segment contract to return.
var generatedKinds = []string{"seam", "musing", "daypart_transition", "wake_greeting"}

func TestRulesForCoversEveryGeneratedKind(t *testing.T) {
	for _, k := range generatedKinds {
		rules, ok := RulesFor(k)
		require.True(t, ok, k)
		require.NotEmpty(t, rules, k)
	}
}

func TestRulesForRejectsWhatTheBrainDoesNotWrite(t *testing.T) {
	for _, k := range []string{"station_id", "", "dj", "Seam", "dedication_read"} {
		_, ok := RulesFor(k)
		require.False(t, ok, k)
	}
}

func TestEveryKindGetsItsOwnRules(t *testing.T) {
	seen := map[string]string{}
	for _, k := range generatedKinds {
		rules, _ := RulesFor(k)
		if prev, dup := seen[rules]; dup {
			t.Fatalf("%s and %s share one rules block", prev, k)
		}
		seen[rules] = k
	}
}

// The three anchor-free kinds have no just_played and no coming_up in their
// brief. A rule that names an absent field is how an invented promise gets on
// air - she would open a track the director never pinned.
func TestAnchorFreeRulesNameNoTrackFields(t *testing.T) {
	for _, k := range []string{"musing", "daypart_transition", "wake_greeting"} {
		rules, _ := RulesFor(k)
		require.NotContains(t, rules, "coming_up", k)
		require.NotContains(t, rules, "just_played", k)
	}
}

// Digit-lint is enforced post hoc by Validate, but every rule must also SAY
// it - a retry costs a whole extra model call.
func TestEveryRuleForbidsNumerals(t *testing.T) {
	for _, k := range generatedKinds {
		rules, _ := RulesFor(k)
		require.Contains(t, rules, "viết bằng chữ", k)
	}
}
