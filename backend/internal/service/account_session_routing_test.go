package service

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func accountSessionRoutingTestContext(path string, keyID, groupID int64, platform string) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", path, nil)
	c.Set("api_key", &APIKey{ID: keyID, GroupID: &groupID, Group: &Group{ID: groupID, Platform: platform}})
	return c
}

func accountSessionRoutingTestAffinity(t *testing.T, c *gin.Context) AccountSessionAffinity {
	t.Helper()
	affinity, ok := AccountSessionAffinityFromContext(c.Request.Context())
	require.True(t, ok)
	require.Len(t, affinity.Key, 64)
	return affinity
}

func TestAccountSessionAffinityContextClearAndReplace(t *testing.T) {
	_, ok := AccountSessionAffinityFromContext(nil)
	require.False(t, ok)
	ctx, cancel := context.WithCancel(context.Background())
	ctx = WithAccountSessionAffinity(ctx, AccountSessionAffinity{Key: "first"})
	ctx = WithAccountSessionAffinity(ctx, AccountSessionAffinity{Key: "next", PersistentRoute: true})
	affinity, ok := AccountSessionAffinityFromContext(ctx)
	require.True(t, ok)
	require.Equal(t, "next", affinity.Key)
	require.True(t, affinity.PersistentRoute)
	ctx = WithAccountSessionAffinity(ctx, AccountSessionAffinity{})
	_, ok = AccountSessionAffinityFromContext(ctx)
	require.False(t, ok)
	cancel()
	require.ErrorIs(t, ctx.Err(), context.Canceled)
}

func TestBindOpenAIAccountSessionAffinityExplicitScope(t *testing.T) {
	bind := func(keyID, groupID int64, platform string, headers map[string]string, body string) AccountSessionAffinity {
		c := accountSessionRoutingTestContext("/v1/responses", keyID, groupID, platform)
		for name, value := range headers {
			c.Request.Header.Set(name, value)
		}
		BindOpenAIAccountSessionAffinity(c, []byte(body), false, "")
		return accountSessionRoutingTestAffinity(t, c)
	}
	headers := map[string]string{"session-id": "conversation-private-value"}
	first := bind(11, 21, PlatformOpenAI, headers, `{"input":"first prompt"}`)
	require.False(t, first.PersistentRoute, "HTTP identity requires observed reuse before reserving capacity")
	require.NotContains(t, first.Key, "private")
	require.Equal(t, first, bind(11, 21, PlatformOpenAI, headers, `{"input":"later prompt"}`))
	require.NotEqual(t, first.Key, bind(12, 21, PlatformOpenAI, headers, `{}`).Key)
	require.NotEqual(t, first.Key, bind(11, 22, PlatformOpenAI, headers, `{}`).Key)
	require.NotEqual(t, first.Key, bind(11, 21, PlatformGrok, headers, `{}`).Key)
	cacheFirst := bind(11, 21, PlatformOpenAI, nil, `{"prompt_cache_key":"cache-private","input":"one"}`)
	require.Equal(t, cacheFirst, bind(11, 21, PlatformOpenAI, nil, `{"prompt_cache_key":"cache-private","input":"two"}`))
}

func TestGenerateSessionHashContentFallbackDoesNotReserveCapacity(t *testing.T) {
	c := accountSessionRoutingTestContext("/v1/responses", 11, 21, PlatformOpenAI)
	c.Request = c.Request.WithContext(WithAccountSessionAffinity(c.Request.Context(), AccountSessionAffinity{Key: "old-session"}))
	svc := &OpenAIGatewayService{}
	require.NotEmpty(t, svc.GenerateSessionHash(c, []byte(`{"model":"gpt-test","stream":true,"input":"independent prompt"}`)))
	_, ok := AccountSessionAffinityFromContext(c.Request.Context())
	require.False(t, ok, "content affinity is useful for cache routing but must not reserve conversation capacity")
	c.Request.Header.Set("session-id", "explicit")
	require.NotEmpty(t, svc.GenerateSessionHash(c, []byte(`{"input":"independent prompt"}`)))
	require.NotEmpty(t, accountSessionRoutingTestAffinity(t, c).Key)
}

func TestBindOpenAIAccountSessionAffinitySeparatesThreadsAndLanes(t *testing.T) {
	bind := func(thread, kind string) string {
		c := accountSessionRoutingTestContext("/v1/responses", 11, 21, PlatformOpenAI)
		c.Request.Header.Set("session-id", "shared-parent-session")
		c.Request.Header.Set("thread-id", thread)
		c.Request.Header.Set("x-codex-turn-metadata", `{"thread_id":"`+thread+`","request_kind":"`+kind+`"}`)
		BindOpenAIAccountSessionAffinity(c, nil, false, "")
		return accountSessionRoutingTestAffinity(t, c).Key
	}
	turn := bind("parent", "turn")
	require.NotEqual(t, turn, bind("child", "turn"))
	require.NotEqual(t, turn, bind("parent", "memory"))
	require.Equal(t, turn, bind("parent", "prewarm"))
	require.Equal(t, turn, bind("parent", "compaction"))
}

func TestBindGatewayAccountSessionAffinityMetadataAndFallbackGroup(t *testing.T) {
	c := accountSessionRoutingTestContext("/v1/messages", 11, 21, PlatformAnthropic)
	key, _ := accountSessionRoutingAPIKey(c)
	metadata := FormatMetadataUserID(strings.Repeat("a", 64), "", "123e4567-e89b-12d3-a456-426614174000", "2.1.78")
	parsed := &ParsedRequest{MetadataUserID: metadata}
	BindGatewayAccountSessionAffinity(c, key, parsed, PlatformAnthropic)
	first := accountSessionRoutingTestAffinity(t, c)
	require.False(t, first.PersistentRoute)
	fallbackGroup := int64(22)
	fallbackKey := *key
	fallbackKey.GroupID = &fallbackGroup
	c.Request = c.Request.WithContext(WithResolvedTargetPlatform(c.Request.Context(), PlatformOpenAI))
	BindGatewayAccountSessionAffinity(c, &fallbackKey, parsed, PlatformAnthropic)
	require.NotEqual(t, first.Key, accountSessionRoutingTestAffinity(t, c).Key)
	fresh := accountSessionRoutingTestContext("/v1/messages", 11, fallbackGroup, PlatformAnthropic)
	BindGatewayAccountSessionAffinity(fresh, &fallbackKey, parsed, PlatformAnthropic)
	require.Equal(t, accountSessionRoutingTestAffinity(t, fresh), accountSessionRoutingTestAffinity(t, c), "fallback group routing must not retain the previous composite target platform")
	BindGatewayAccountSessionAffinity(c, &fallbackKey, &ParsedRequest{}, PlatformAnthropic)
	_, ok := AccountSessionAffinityFromContext(c.Request.Context())
	require.False(t, ok)
}

func TestBindOpenAIAccountSessionAffinityIgnoresResponseIDs(t *testing.T) {
	c := accountSessionRoutingTestContext("/v1/responses", 11, 21, PlatformGrok)
	BindOpenAIAccountSessionAffinity(c, []byte(`{"previous_response_id":"resp_previous"}`), false, "")
	_, ok := AccountSessionAffinityFromContext(c.Request.Context())
	require.False(t, ok, "a per-turn response identifier is not a stable conversation identity")
}

func TestBindGatewayAccountSessionAffinityNativeSeedKeepsIdentityValidation(t *testing.T) {
	c := accountSessionRoutingTestContext("/v1beta/models/gemini-test:generateContent", 11, 21, PlatformGemini)
	key, _ := accountSessionRoutingAPIKey(c)
	BindGatewayAccountSessionAffinity(c, key, nil, PlatformGemini, "gemini-cli-identity")
	first := accountSessionRoutingTestAffinity(t, c)
	require.False(t, first.PersistentRoute)
	c.Request.Header.Set("session-id", "header-session")
	parsed := &ParsedRequest{Body: NewRequestBodyRef([]byte(`{"client_metadata":{"session_id":"conflicting-body-session"}}`))}
	BindGatewayAccountSessionAffinity(c, key, parsed, PlatformGemini, "gemini-cli-identity")
	_, ok := AccountSessionAffinityFromContext(c.Request.Context())
	require.False(t, ok, "a native identity must not revive rejected client identity")
}

func TestBindOpenAIAccountSessionAffinityWebSocketTurns(t *testing.T) {
	c := accountSessionRoutingTestContext("/v1/responses", 11, 21, PlatformOpenAI)
	BindOpenAIAccountSessionAffinity(c, []byte(`{"input":"one"}`), true, "connection-one")
	anonymous := accountSessionRoutingTestAffinity(t, c)
	require.True(t, anonymous.PersistentRoute)
	BindOpenAIAccountSessionAffinity(c, []byte(`{"input":"next"}`), true, "connection-one")
	require.Equal(t, anonymous, accountSessionRoutingTestAffinity(t, c))
	other := accountSessionRoutingTestContext("/v1/responses", 11, 21, PlatformOpenAI)
	BindOpenAIAccountSessionAffinity(other, nil, true, "connection-two")
	require.NotEqual(t, anonymous.Key, accountSessionRoutingTestAffinity(t, other).Key)
	BindOpenAIAccountSessionAffinity(c, []byte(`{"client_metadata":{"thread_id":"new-thread"}}`), true, "connection-one")
	thread := accountSessionRoutingTestAffinity(t, c)
	require.NotEqual(t, anonymous.Key, thread.Key)
	BindOpenAIAccountSessionAffinity(c, []byte(`{"input":"identity omitted on next frame"}`), true, "connection-one")
	require.Equal(t, thread, accountSessionRoutingTestAffinity(t, c))
	key, _ := accountSessionRoutingAPIKey(c)
	nextKey := *key
	nextKey.ID++
	c.Set("api_key", &nextKey)
	BindOpenAIAccountSessionAffinity(c, nil, true, "connection-one")
	require.NotEqual(t, thread.Key, accountSessionRoutingTestAffinity(t, c).Key)
	c.Request.Header.Set("session-id", "header-session")
	BindOpenAIAccountSessionAffinity(c, []byte(`{"client_metadata":{"session_id":"conflicting-body-session"}}`), true, "connection-one")
	_, ok := AccountSessionAffinityFromContext(c.Request.Context())
	require.False(t, ok, "a conflicting turn must clear the old affinity")
}

func TestBindAccountSessionAffinityExcludedRoutesAndForcedPlatform(t *testing.T) {
	for _, path := range []string{"/v1/messages/count_tokens", "/v1/responses/input_tokens", "/v1/images/generations", "/v1/embeddings", "/v1/alpha/search"} {
		t.Run(path, func(t *testing.T) {
			c := accountSessionRoutingTestContext(path, 11, 21, PlatformOpenAI)
			c.Request.Header.Set("session-id", "known-session")
			c.Request = c.Request.WithContext(WithAccountSessionAffinity(c.Request.Context(), AccountSessionAffinity{Key: "previous-turn"}))
			BindOpenAIAccountSessionAffinity(c, nil, false, "")
			_, ok := AccountSessionAffinityFromContext(c.Request.Context())
			require.False(t, ok)
		})
	}
	c := accountSessionRoutingTestContext("/v1/responses", 11, 21, PlatformOpenAI)
	c.Request.Header.Set("session-id", "known-session")
	BindOpenAIAccountSessionAffinity(c, nil, false, "")
	first := accountSessionRoutingTestAffinity(t, c)
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.ForcePlatform, PlatformAntigravity))
	BindOpenAIAccountSessionAffinity(c, nil, false, "")
	require.NotEqual(t, first.Key, accountSessionRoutingTestAffinity(t, c).Key)
}
