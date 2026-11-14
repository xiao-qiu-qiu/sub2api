package handler

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIChatCompletionsRejectsInvalidReasoningBeforeScheduling(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, reasoning := range []string{`"unknown-private-value"`, `true`, `42`, `[]`, `"low","Reasoning":"high"`, `"low","reasoning":"high"`} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", reasoning, stream), func(t *testing.T) {
				var acquired atomic.Int64
				h := newOpenAIImageChatRejectionHandlerWithCache(t, &concurrencyCacheMock{
					acquireUserSlotFn: func(context.Context, int64, int, string) (bool, error) {
						acquired.Add(1)
						return true, nil
					},
				})
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				body := fmt.Sprintf(`{"model":"gpt-6.1-sol","messages":[{"role":"user","content":"hello"}],"stream":%t,"reasoning":%s}`, stream, reasoning)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
				setImageChatTestAuth(c)

				h.ChatCompletions(c)

				require.Equal(t, http.StatusBadRequest, recorder.Code)
				assert.Equal(t, "invalid_request_error", gjson.GetBytes(recorder.Body.Bytes(), "error.type").String())
				assert.Contains(t, gjson.GetBytes(recorder.Body.Bytes(), "error.message").String(), "reasoning")
				assert.NotContains(t, recorder.Body.String(), "unknown-private-value")
				assert.Zero(t, acquired.Load())
				_, selected := c.Get(opsAccountIDKey)
				assert.False(t, selected)
			})
		}
	}
}

func TestOpenAIChatCompletionsStringReasoningEnforcesGroupPolicy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mappingDeny := range []bool{false, true} {
		t.Run(fmt.Sprintf("mapping-deny=%t", mappingDeny), func(t *testing.T) {
			h := newOpenAIImageChatRejectionHandler(t)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(
				`{"model":"gpt-6.1-sol","messages":[{"role":"user","content":"hello"}],"reasoning":"high","reasoning_effort":"low"}`,
			))
			group := &service.Group{
				Platform:                    service.PlatformOpenAI,
				MaxReasoningEffort:          "medium",
				MaxReasoningEffortOverLimit: service.ReasoningEffortOverLimitDeny,
			}
			if mappingDeny {
				group.MaxReasoningEffort = ""
				group.ReasoningEffortMappings = []service.ReasoningEffortMapping{{From: "high", To: service.ReasoningEffortMappingDeny}}
			}
			key := &service.APIKey{ID: 4348, UserID: 4348, User: &service.User{ID: 4348}, Group: group}
			c.Set(string(middleware.ContextKeyAPIKey), key)
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: key.UserID, Concurrency: 1})

			h.ChatCompletions(c)

			require.Equal(t, http.StatusForbidden, recorder.Code)
			assert.Equal(t, "permission_error", gjson.GetBytes(recorder.Body.Bytes(), "error.type").String())
			requested := service.RequestedReasoningEffortFromContext(c.Request.Context())
			require.NotNil(t, requested)
			assert.Equal(t, "high", *requested)
			_, selected := c.Get(opsAccountIDKey)
			assert.False(t, selected)
		})
	}
}
