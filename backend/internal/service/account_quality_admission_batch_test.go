package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestQualityAdmissionSharedBatchNeedsEvidenceForEveryModel(t *testing.T) {
	batch, err := newQualitySharedBatch(time.Now().Add(-5 * time.Minute))
	require.NoError(t, err)
	wrong := sharedBatchFixture(t, batch, 1, "incorrect")
	other := sharedBatchFixture(t, batch, 1, "incorrect")
	other.Results[0].PelicanConfig.ModelID = "other-model"
	other.Results[0].PelicanConfig.QualityBatch.MappedModel = "other-model"
	wrong.Plan.PelicanConfig.ModelIDs = []string{"test-model", "other-model"}
	wrong.Results = append(wrong.Results, other.Results[0])
	passed := sharedBatchFixture(t, batch, 2, "correct")
	peers := []*QualityBatchRun{wrong, passed}

	SetQualityProbeFinishedAt(wrong.Plan, wrong.Results)
	require.False(t, wrong.Plan.QualityProbeAdmissionChecked, "collection alone cannot release admission")
	require.Equal(t, "failed", EvaluateQualityBatch(wrong, peers))
	require.Equal(t, "peer_passed", wrong.Results[0].PelicanConfig.QualityBatch.Decision)
	require.Equal(t, "no_passing_peer", wrong.Results[1].PelicanConfig.QualityBatch.Decision)
	SetQualityProbeFinishedAt(wrong.Plan, wrong.Results)
	require.False(t, wrong.Plan.QualityProbeAdmissionChecked, "a conclusive failed model cannot admit another inconclusive model")

	otherPassed := sharedBatchFixture(t, batch, 3, "correct")
	otherPassed.Results[0].PelicanConfig.ModelID = "other-model"
	otherPassed.Results[0].PelicanConfig.QualityBatch.MappedModel = "other-model"
	peers = append(peers, otherPassed)
	require.Equal(t, "failed", EvaluateQualityBatch(wrong, peers))
	SetQualityProbeFinishedAt(wrong.Plan, wrong.Results)
	require.True(t, wrong.Plan.QualityProbeAdmissionChecked, "a complete conclusive failure follows the existing isolation policy")
}
