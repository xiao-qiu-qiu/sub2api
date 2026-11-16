package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
)

// AccountSessionAffinity identifies a conversation for short-lived soft capacity
// accounting. It never changes sticky routing or owns a real concurrency slot.
type AccountSessionAffinity struct {
	Key             string
	PersistentRoute bool
}

type accountSessionAffinityContextKey struct{}

// WithAccountSessionAffinity also accepts an empty value to clear an affinity
// inherited from an earlier WebSocket turn or a different routing group.
func WithAccountSessionAffinity(ctx context.Context, affinity AccountSessionAffinity) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, accountSessionAffinityContextKey{}, affinity)
}

func AccountSessionAffinityFromContext(ctx context.Context) (AccountSessionAffinity, bool) {
	if ctx == nil {
		return AccountSessionAffinity{}, false
	}
	affinity, ok := ctx.Value(accountSessionAffinityContextKey{}).(AccountSessionAffinity)
	return affinity, ok && affinity.Key != ""
}

// BindOpenAIAccountSessionAffinity follows explicit client identity, independently
// of the content-derived sticky hash. Ordinary HTTP becomes stable only after
// repeated admissions; only an actual WebSocket sets PersistentRoute.
func BindOpenAIAccountSessionAffinity(c *gin.Context, body []byte, persistentRoute bool, fallbackSeed string) {
	if c == nil || c.Request == nil {
		return
	}
	apiKey, _ := accountSessionRoutingAPIKey(c)
	bindAccountSessionAffinity(c, apiKey, body, "", "", persistentRoute, fallbackSeed)
}

// BindGatewayAccountSessionAffinity covers the native Anthropic/Gemini handlers.
// The current key is explicit because the invalid-request fallback may schedule
// through a cloned key whose group differs from the authentication context.
func BindGatewayAccountSessionAffinity(c *gin.Context, apiKey *APIKey, parsed *ParsedRequest, platform string, explicitSessionSeed ...string) {
	var body []byte
	var metadataUserID string
	if parsed != nil {
		body = parsed.Body.Bytes()
		metadataUserID = parsed.MetadataUserID
	}
	bindAccountSessionAffinity(c, apiKey, body, metadataUserID, platform, false, "")
	if c != nil && c.Request != nil && len(explicitSessionSeed) > 0 && explicitSessionSeed[0] != "" && apiKey != nil && apiKey.ID > 0 && accountSessionAffinityTextRoute(c.Request.URL.Path) {
		if _, valid := accountSessionIdentitySeed(c, body, metadataUserID); valid {
			setAccountSessionRoutingIdentity(c, apiKey, platform, "native:"+explicitSessionSeed[0], false)
		}
	}
}

func accountSessionRoutingAPIKey(c *gin.Context) (*APIKey, bool) {
	if c == nil {
		return nil, false
	}
	value, ok := c.Get("api_key")
	key, valid := value.(*APIKey)
	return key, ok && valid && key != nil
}

func bindAccountSessionAffinity(c *gin.Context, apiKey *APIKey, body []byte, metadataUserID, platform string, persistentRoute bool, fallbackSeed string) {
	if c == nil || c.Request == nil {
		return
	}
	affinity := AccountSessionAffinity{}
	if apiKey != nil && apiKey.ID > 0 && accountSessionAffinityTextRoute(c.Request.URL.Path) {
		seed, valid := accountSessionIdentitySeed(c, body, metadataUserID)
		if persistentRoute && !valid {
			c.Set("account_session_persistent_identity", "")
		}
		if valid && persistentRoute {
			// Frames may omit an identity established by the first frame. Keep
			// that connection identity while rebuilding its tenant/group scope.
			if seed == "" {
				seed = c.GetString("account_session_persistent_identity")
			}
			if seed == "" && fallbackSeed != "" {
				seed = "connection:" + fallbackSeed
			}
			if seed != "" {
				c.Set("account_session_persistent_identity", seed)
			}
		}
		if seed != "" {
			setAccountSessionRoutingIdentity(c, apiKey, platform, seed, persistentRoute)
			return
		}
	}
	c.Request = c.Request.WithContext(WithAccountSessionAffinity(c.Request.Context(), affinity))
}

func setAccountSessionRoutingIdentity(c *gin.Context, apiKey *APIKey, platform, seed string, persistentRoute bool) {
	if platform == "" {
		platform = PlatformFromAPIKey(apiKey)
		if resolved, ok := ResolvedTargetPlatformFromContext(c.Request.Context()); ok {
			platform = resolved
		}
		if forced, ok := c.Request.Context().Value(ctxkey.ForcePlatform).(string); ok && forced != "" {
			platform = forced
		}
	}
	encoded := fmt.Sprintf("account_session:v1:%d:%d:%s:%s", apiKey.ID, derefGroupID(apiKey.GroupID), platform, seed)
	sum := sha256.Sum256([]byte(encoded))
	affinity := AccountSessionAffinity{Key: hex.EncodeToString(sum[:]), PersistentRoute: persistentRoute}
	c.Request = c.Request.WithContext(WithAccountSessionAffinity(c.Request.Context(), affinity))
}

func accountSessionIdentitySeed(c *gin.Context, body []byte, metadataUserID string) (string, bool) {
	resolution := resolveOpenAIClientSessionIdentity(c, body)
	if resolution.metadata.Status == OpenAIClientSessionIdentityInvalid || resolution.metadata.Status == OpenAIClientSessionIdentityConflict {
		return "", false
	}
	lane := resolveOpenAIWSExecutionLane(c, body)
	// Reuse execution lanes so Codex memory/background agents cannot consume
	// the user's conversation lease merely because session-id is shared.
	if thread := resolveOpenAIWSClientThreadID(c, body); thread != "" {
		return "thread:" + thread + "|" + lane, true
	}
	if resolution.metadata.Status == OpenAIClientSessionIdentityResolved {
		return resolution.identity.kind + ":" + resolution.identity.value + "|" + lane, true
	}
	if session := explicitOpenAISessionID(c, body); session != "" {
		return "session:" + session + "|" + lane, true
	}
	if isGrokRequestContext(c) {
		if session := strings.TrimSpace(c.GetHeader(grokConversationIDHeader)); session != "" {
			return "grok:" + session + "|" + lane, true
		}
	}
	if session := ClaudeCodeSessionIDFromHeader(c); session != "" {
		return "claude:" + session + "|" + lane, true
	}
	if metadataUserID == "" && len(body) > 0 {
		metadataUserID = openAIRequestPayloadView(body).Get("metadata.user_id").String()
	}
	if metadata := ParseMetadataUserID(metadataUserID); metadata != nil && metadata.SessionID != "" {
		return "claude:" + metadata.SessionID + "|" + lane, true
	}
	return "", true
}

func accountSessionAffinityTextRoute(path string) bool {
	path = strings.ToLower(strings.TrimRight(path, "/"))
	if strings.Contains(path, "/count_tokens") || strings.Contains(path, "/input_tokens") {
		return false
	}
	return strings.Contains(path, "/responses") || strings.HasSuffix(path, "/messages") ||
		strings.HasSuffix(path, "/chat/completions") || strings.HasSuffix(path, ":generatecontent") ||
		strings.HasSuffix(path, ":streamgeneratecontent")
}
