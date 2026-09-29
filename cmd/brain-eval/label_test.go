package main

import "testing"

// The labeler with fixed reply texts (E10-S2 AC): S73, negation, refusal plus claim, and the
// plain cases of each label. The incomplete reply is in TestResultStatus: it is a run status,
// and its label is UNSURE before any rule runs.
func TestLabel(t *testing.T) {
	update := Check{Expect: []string{"bun"}, Stale: []string{"pnpm"}}
	marker := Check{Expect: []string{"kest-1234"}}
	decline := Check{Stale: []string{"kest-1234"}, Decline: true}
	tests := []struct {
		name  string
		reply string
		check Check
		want  Label
	}{
		{"expected word", "bun", update, Pass},
		{"expected word in a sentence", "We use Bun here.", update, Pass},
		{"expected marker", "The code word is kest-1234.", marker, Pass},
		{"stale word", "pnpm", update, Stale},
		{"S73: expected and stale", "bun (it was pnpm before)", update, Unsure},
		{"S73: stale and expected", "pnpm, or maybe bun", update, Unsure},
		{"negation", "not bun", update, Unsure},
		{"negation of the stale answer", "Use bun, not pnpm.", update, Unsure},
		{"negation two words before", "We don't use bun.", update, Unsure},
		{"typographic apostrophe", "We don’t use bun.", update, Unsure},
		{"negation too far away", "No lockfile says so, but we use bun.", update, Pass},
		{"refusal plus claim", "I don't know, maybe bun.", update, Unsure},
		{"refusal plus stale claim", "I'm not sure. pnpm?", update, Unsure},
		{"refusal", "I don't know which package manager you use.", update, Miss},
		{"another answer", "yarn", update, Miss},
		{"a longer word is no answer", "bundle", update, Miss},
		{"a longer marker is no answer", "kest-12345", marker, Miss},
		{"empty reply", "", update, Miss},
		{"decline: refusal", "I do not know the deploy code.", decline, Pass},
		{"decline: refusal plus claim", "I'm not sure, perhaps kest-1234.", decline, Unsure},
		{"decline: claim", "The deploy code is kest-1234.", decline, Invented},
		{"decline: neither", "The deploy code is zorp-9999.", decline, Unsure},
		{"decline: negated claim", "It is not kest-1234.", decline, Unsure},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, rule := label(tt.reply, tt.check)
			if got != tt.want {
				t.Errorf("label(%q) = %s (%s), want %s", tt.reply, got, rule, tt.want)
			}
			if rule == "" {
				t.Errorf("label(%q) names no rule", tt.reply)
			}
		})
	}
}
