package httpapi

import (
	"testing"

	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/store"
)

// A person may ask for what the incident's profiles allow, with the pods and
// containers its evidence names to choose from, while it takes evidence.
func TestCollectable(t *testing.T) {
	cat, err := catalog.Themis(nil)
	if err != nil {
		t.Fatal(err)
	}
	items, _ := uiShop(t, cat)
	api := &API{Collect: uiCollector{}, Config: config.Config{Profiles: cat.Profiles}}
	in := store.Incident{Profile: "oom-limit-too-low", State: incident.AwaitingSuggestion}

	got := api.collectable(in, items)
	byTemplate := map[string]collectView{}
	for _, c := range got {
		byTemplate[c.Template] = c
	}
	if len(got) != 7 || byTemplate[evidence.ContainerCPU].Template != "" {
		t.Errorf("collectable = %+v, want the 7 templates the profile allows", got)
	}
	logs := byTemplate[evidence.ContainerLogsTail]
	if len(logs.Params) != 3 || len(logs.Params[0].Choices) != 2 || logs.Params[0].Choices[0].Value != shopPod ||
		logs.Params[1].Choices[0].Value != "app" || logs.Params[2].Choices[0] != (choiceView{"true", "Previous run"}) {
		t.Errorf("logs = %+v", logs)
	}
	if w := byTemplate[evidence.ContainerMemory].Params; len(w) != 1 || w[0].Name != evidence.WindowParam || w[0].Choices[2].Value != "168h" {
		t.Errorf("memory = %+v", w)
	}

	for _, state := range []incident.State{incident.Received, incident.Proposed, incident.Executing, incident.Verifying, incident.Resolved, incident.ClosedNoAction} {
		in.State = state
		if got := api.collectable(in, items); got != nil {
			t.Errorf("%s offers %+v", state, got)
		}
	}
	in.State = incident.Escalated
	if got := (&API{Config: api.Config}).collectable(in, items); got != nil {
		t.Errorf("without a collector = %+v", got)
	}
}
