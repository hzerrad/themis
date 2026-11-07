package httpapi

import (
	"slices"

	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/store"
)

// maxFacts is how many facts open an incident's page: enough to say what
// fails and what decides the fix, few enough to read at a glance.
const maxFacts = 4

// factView is one fact the checks found, as a sentence, with the evidence it
// rests on.
type factView struct {
	Words phrase     `json:"words"`
	Cites []citeView `json:"cites,omitempty"`
}

// citeView points at a piece of evidence by its place in the list.
type citeView struct {
	N    int    `json:"n"`
	Kind string `json:"kind"`
}

// facts are what the incident's checks found, as their checks write it, most
// important first: the failure its profiles' symptoms find, then the checks
// that decide a fix, as the profiles justify fixes and their policies read
// checks, then the profiles' other checks. A check that says only whether
// figures are there is left out: the findings say what the others rest on.
// The same sentence from two checks shows once.
func (a *API) facts(in store.Incident, results []check.Result, cite func([]string) []citeView) []factView {
	var out []factView
	var said []string
	for _, name := range a.weighed(in, results) {
		i := slices.IndexFunc(results, func(r check.Result) bool { return r.Name == name })
		if i < 0 || results[i].Fact == "" || slices.Contains(said, results[i].Fact) {
			continue
		}
		if c, ok := a.CheckList.Get(name); ok && c.Availability {
			continue
		}
		said = append(said, results[i].Fact)
		out = append(out, factView{Words: written(results[i].Fact), Cites: cite(results[i].EvidenceIDs)})
		if len(out) == maxFacts {
			break
		}
	}
	return out
}

// weighed are the checks of results by how much they matter for the
// incident, as facts orders them.
func (a *API) weighed(in store.Incident, results []check.Result) []string {
	var out []string
	add := func(names ...string) {
		for _, n := range names {
			if !slices.Contains(out, n) {
				out = append(out, n)
			}
		}
	}
	matched := a.matched(in)
	for _, name := range matched.Symptoms() {
		if slices.ContainsFunc(results, func(r check.Result) bool { return r.Name == name && r.Value == check.True }) {
			add(name)
		}
	}
	for _, p := range matched {
		for _, fix := range p.Capabilities {
			add(p.JustifiedBy[fix]...)
		}
	}
	for _, p := range matched {
		if reads, err := a.decider.Reads(p); err == nil {
			add(reads...)
		}
	}
	for _, p := range matched {
		add(p.Checks...)
	}
	for _, r := range results {
		add(r.Name)
	}
	return out
}

// situation is what fails, in a few words, from the first of the
// incident's symptoms its checks found: the page's title while nobody has
// proposed a fix.
func (a *API) situation(in store.Incident, results []check.Result) phrase {
	for _, name := range a.matched(in).Symptoms() {
		for _, r := range results {
			if r.Name == name && r.Value == check.True && r.Headline != "" {
				return written(r.Headline)
			}
		}
	}
	return nil
}

// symptoms are the checks of the incident's profiles that find its
// failure.
func (a *API) symptoms(in store.Incident) []string {
	return a.matched(in).Symptoms()
}

// matched are the incident's profiles still configured; none without the
// configuration.
func (a *API) matched(in store.Incident) config.Matched {
	if a.Config == nil {
		return nil
	}
	return a.Config.Current().Matched(in.Profiles())
}
