package service

import (
	"context"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type deferredExcelBPSReasoningPolicyKey struct{}

// WithDeferredExcelBPSReasoningPolicyError retains an ingress denial until the
// selected route is known. Only BPS may replace it by validating a newer history
// effort against the same policy; other routes must return the original denial.
func WithDeferredExcelBPSReasoningPolicyError(ctx context.Context, err error) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if !IsReasoningEffortPolicyDenied(err) {
		return ctx
	}
	return context.WithValue(ctx, deferredExcelBPSReasoningPolicyKey{}, err)
}

func DeferredExcelBPSReasoningPolicyError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	err, _ := ctx.Value(deferredExcelBPSReasoningPolicyKey{}).(error)
	return err
}

func guardDeferredExcelBPSReasoningPolicy(ctx context.Context, c *gin.Context, bpsSelected bool) error {
	err := DeferredExcelBPSReasoningPolicyError(ctx)
	if err == nil || bpsSelected {
		return nil
	}
	MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalPolicyDenied)
	c.JSON(http.StatusForbidden, gin.H{"error": gin.H{"type": "permission_error", "message": err.Error()}})
	return err
}

// Resolve BPS-local history before agent and image processing. The HTTP ingress
// already enforced the top-level effort, so replay its policy only when history
// supplies a new value; mappings need not be idempotent.
func prepareExcelBPSConfiguration(ctx context.Context, body []byte) (context.Context, []byte, error) {
	resolved, _, effortUpdated, err := basispoints.ResolveConfigurationUpdatesWithEffort(body)
	if err != nil {
		return ctx, body, err
	}
	if !effortUpdated {
		return ctx, resolved, DeferredExcelBPSReasoningPolicyError(ctx)
	}
	if denied := DeferredExcelBPSReasoningPolicyError(ctx); denied != nil {
		if _, bound := ctx.Value(openAIReasoningEffortPolicyContextKey{}).(openAIReasoningEffortPolicy); !bound {
			return ctx, body, denied
		}
	}

	// Keep the latest client preference separate from key/group rewrites and
	// BPS's max/ultra -> xhigh normalization. Keep this context local to the BPS
	// attempt so a later native failover retains the original ingress semantics.
	requested := strings.ToLower(strings.TrimSpace(gjson.GetBytes(resolved, "reasoning.effort").String()))
	if canonical := NormalizeMaxReasoningEffort(requested); canonical != "" {
		requested = canonical
	}
	ctx = WithRequestedReasoningEffort(ctx, requested)
	updated, _, err := ApplyOpenAIReasoningEffortPolicyFromContext(ctx, resolved)
	if err != nil {
		return ctx, body, err
	}
	return ctx, updated, nil
}
