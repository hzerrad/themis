package investigation

import (
	"context"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/store"
)

// maxGapDetail bounds what a gap says, whoever said it.
const maxGapDetail = 1024

// recordGap is the one rule for keeping a gap, whoever met it: Themis
// preparing the investigation, Themis reading evidence under a lease, or the
// investigator reporting it. A gap is kept only for a template Themis has and
// a cause it knows, with its detail bounded; the store keeps one per lease,
// template and cause. It reports whether g was kept.
func recordGap(ctx context.Context, s *store.Store, tx pgx.Tx, templates evidence.Registry, g store.EvidenceGap) (bool, error) {
	if _, ok := templates[g.Template]; !ok || !slices.Contains(evidence.GapCauses(), g.Cause) {
		return false, nil
	}
	g.Detail = truncate(g.Detail, maxGapDetail)
	return true, s.InsertEvidenceGap(ctx, tx, g)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}
