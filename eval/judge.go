package eval

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/incident/governance/policy"
)

// Proposal is a fix a model proposed for a case, for Judge.
type Proposal struct {
	// Scenario names the case.
	Scenario string `json:"scenario"`
	Fix
}

// Verdict is what Themis's core does with a proposal, and whether it is the
// case's fix.
type Verdict struct {
	// Outcome is allow, require_approval or deny. A fix the incident's
	// profiles don't offer, or one that can't go ahead, reaches nobody: deny.
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
	// SameChange is set when the proposal changes what the case's fix
	// changes, the same way: the same fields of the same containers, each
	// raised or lowered as the case's fix does. Values are the
	// investigator's to choose.
	SameChange bool `json:"same_change"`
}

// Judge decides each proposal against its case, among the scenario packs
// under root, as Themis's core would decide it: the replay can't, so the
// evaluation asks this after its runs. The proposal is taken to name the
// case's target; the evaluation scores another target itself.
func Judge(ctx context.Context, root string, proposals []Proposal) ([]Verdict, error) {
	paths, err := filepath.Glob(filepath.Join(root, "*", "eval", "*", "case.yaml"))
	if err != nil {
		return nil, err
	}
	dirs := map[string]string{}
	for _, p := range paths {
		dirs[filepath.Base(filepath.Dir(p))] = filepath.Dir(p)
	}
	packs := os.DirFS(root)
	cat, err := catalog.Build(nil, nil, packs, nil)
	if err != nil {
		return nil, err
	}
	type judged struct {
		s Case
		f found
	}
	cases := map[string]judged{}
	out := make([]Verdict, 0, len(proposals))
	for _, p := range proposals {
		j, ok := cases[p.Scenario]
		if !ok {
			dir, ok := dirs[p.Scenario]
			if !ok {
				return nil, fmt.Errorf("no case is named %s", p.Scenario)
			}
			pack := filepath.Base(filepath.Dir(filepath.Dir(dir)))
			profile, _ := cat.Profile(pack)
			var causes []string
			for _, c := range cat.RootCauses.For(pack) {
				causes = append(causes, c.Name)
			}
			if j.s, err = Load(dir, causes); err != nil {
				return nil, fmt.Errorf("%s: %w", p.Scenario, err)
			}
			if j.f, err = find(ctx, dir, j.s, packs, profile); err != nil {
				return nil, fmt.Errorf("%s: %w", p.Scenario, err)
			}
			cases[p.Scenario] = j
		}
		v, err := j.f.judge(ctx, j.s, p.Fix)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p.Scenario, err)
		}
		out = append(out, v)
	}
	return out, nil
}

// judge decides p as the core decides a suggestion for this incident, and
// compares it with the case's fix.
func (f found) judge(ctx context.Context, s Case, p Fix) (Verdict, error) {
	fix, ok := f.cat.Fixes[p.Capability]
	if !ok || len(f.matched.Offering(p.Capability)) == 0 {
		return Verdict{Outcome: string(policy.Deny), Reason: "None of the incident's profiles offers this fix."}, nil
	}
	params, err := fix.Validate(p.Params)
	if err != nil {
		return Verdict{Outcome: string(policy.Deny), Reason: "Its parameters aren't valid: " + err.Error() + "."}, nil
	}
	decided, err := f.govern(ctx, fix, params)
	if errors.Is(err, capability.ErrInvalidParams) {
		return Verdict{Outcome: string(policy.Deny), Reason: "Its parameters aren't valid."}, nil
	}
	if err != nil {
		return Verdict{}, err
	}
	v := Verdict{Outcome: decided.Outcome, Reason: decided.Reason}
	want, err := f.caseFix(ctx, s)
	if err != nil {
		return Verdict{}, err
	}
	if want != nil && want.Capability == p.Capability {
		v.SameChange = f.sameChange(ctx, fix, want.Params, params)
	}
	return v, nil
}

// sameChange says whether params change what want does, the same way. A
// plan that can't be made changes nothing.
func (f found) sameChange(ctx context.Context, fix capability.Capability, want, params map[string]any) bool {
	a, err := fix.Plan(ctx, f.c, f.target, want)
	if err != nil {
		return false
	}
	b, err := fix.Plan(ctx, f.c, f.target, params)
	if err != nil {
		return false
	}
	return maps.Equal(shape(a.Facts), shape(b.Facts))
}

// shape is what a plan's facts say it changes and which way, without the
// values: less the values before and after, and how far they move. A
// rollback's facts say nothing, so any two rollbacks have the same shape:
// truth.acceptable_revisions judges which revision.
func shape(facts map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range facts {
		if strings.HasSuffix(k, ".before") || strings.HasSuffix(k, ".after") || strings.HasSuffix(k, ".ratio") {
			continue
		}
		if _, err := strconv.ParseFloat(v, 64); err == nil {
			continue
		}
		out[k] = v
	}
	return out
}
