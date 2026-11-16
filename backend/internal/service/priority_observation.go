package service

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"strings"
)

type observationEffortKey struct{}

func WithObservationEffort(ctx context.Context, effort string) context.Context {
	return context.WithValue(ctx, observationEffortKey{}, requesttiming.NormalizeObservationEffort(effort))
}
func observationEffort(ctx context.Context) (string, bool) {
	value, ok := ctx.Value(observationEffortKey{}).(string)
	return value, ok
}
func WithObservationRequestEffort(ctx context.Context, body []byte) context.Context {
	value := ""
	if effort := CanonicalRequestedReasoningEffort(body); effort != nil {
		value = *effort
	}
	return WithObservationEffort(ctx, value)
}

// Observation identity is captured before channel routing. It must never drive
// upstream model support, account mapping, pricing, or response ownership.
type priorityObservationContextKey struct{}
type priorityObservationIdentity struct {
	model, endpoint string
	groupID         int64
	hasGroup        bool
}

func WithPrioritySchedulingObservation(ctx context.Context, group *int64, publicModel, endpoint string) context.Context {
	identity := priorityObservationIdentity{model: strings.TrimSpace(publicModel), endpoint: strings.TrimSpace(endpoint)}
	if group != nil {
		identity.groupID, identity.hasGroup = *group, true
	}
	return context.WithValue(ctx, priorityObservationContextKey{}, identity)
}

func priorityHistoryModel(req OpenAIAccountScheduleRequest) string {
	if model := strings.TrimSpace(req.HistoryModel); model != "" {
		return model
	}
	return strings.TrimSpace(req.RequestedModel)
}

// Public model scopes work before routing; retain legacy routing-model scopes
// as a configuration alias only. Each history/attempt key remains exact.
func priorityApplies(c PrioritySchedulingConfig, req OpenAIAccountScheduleRequest) bool {
	return c.applies(req.GroupID, priorityHistoryModel(req)) || c.applies(req.GroupID, strings.TrimSpace(req.RequestedModel))
}

func priorityObservationFromContext(ctx context.Context, fallback string) priorityObservationIdentity {
	identity, _ := ctx.Value(priorityObservationContextKey{}).(priorityObservationIdentity)
	if identity.model == "" {
		identity.model = strings.TrimSpace(fallback)
		if public, ok := RequestedPublicModelFromContext(ctx); ok {
			identity.model = strings.TrimSpace(public)
		}
	}
	return identity
}

func priorityRequestAttemptKey(req OpenAIAccountScheduleRequest, accountID int64) priorityAttemptKey {
	key := priorityAttemptKey{accountID: accountID, model: priorityHistoryModel(req), endpoint: priorityErrorEndpoint(req), effort: req.ObservationEffort}
	if req.GroupID != nil {
		key.groupID, key.hasGroup = *req.GroupID, true
	}
	return key
}
