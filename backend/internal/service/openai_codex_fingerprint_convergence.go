package service

import (
	"crypto/sha256"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const codexIdentitySchemeExtraKey = "codex_identity_scheme"

// Only an explicit v2 opts into new identity derivation and wire projection.
// Callers with a Shadow must pass the resolved credential source.
func codexIdentityV2Enabled(account *Account) bool {
	if account == nil || !account.IsOpenAIOAuthLike() {
		return false
	}
	scheme, _ := account.Extra[codexIdentitySchemeExtraKey].(string)
	return scheme == "v2"
}

var codexConvergenceIdentityFields = []struct {
	name string
	kind string
}{
	{name: "root_turn_id", kind: "turn"},
	{name: "parent_turn_id", kind: "turn"},
	{name: "parent_thread_id", kind: "thread"},
	{name: "x-codex-parent-thread-id", kind: "thread"},
	{name: "forked_from_thread_id", kind: "thread"},
	{name: "context_window_id", kind: "context_window"},
}

func applyCodexConvergenceIdentityFields(values map[string]any, account *Account, apiKeyID int64) bool {
	if values == nil || !codexIdentityV2Enabled(account) {
		return false
	}
	changed := false
	for _, field := range codexConvergenceIdentityFields {
		if raw, ok := values[field.name].(string); ok && strings.TrimSpace(raw) != "" {
			next := scopeCodexAccountIdentityValue(account, apiKeyID, field.kind, raw)
			if next != raw {
				values[field.name] = next
				changed = true
			}
		}
	}
	return changed
}

func codexIdentitySeedKind(kind string) string {
	switch kind {
	case "session", "prompt-cache", "request":
		return "thread"
	default:
		return kind
	}
}

const codexConvergenceUUIDPattern = `[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`

var (
	codexConvergenceWindowIDPattern       = regexp.MustCompile(`^(` + codexConvergenceUUIDPattern + `):([0-9]+)$`)
	codexConvergencePromptCacheKeyPattern = regexp.MustCompile(`^([A-Za-z0-9_-]{1,64}):(` + codexConvergenceUUIDPattern + `)$`)
)

func deriveCodexIdentityCompositeValue(account *Account, apiKeyID int64, kind, raw string) (string, bool) {
	if !codexIdentityV2Enabled(account) {
		return "", false
	}
	switch kind {
	case "window":
		if match := codexConvergenceWindowIDPattern.FindStringSubmatch(raw); match != nil {
			return scopeCodexAccountIdentityValue(account, apiKeyID, "thread", match[1]) + ":" + match[2], true
		}
	case "prompt-cache":
		if match := codexConvergencePromptCacheKeyPattern.FindStringSubmatch(raw); match != nil {
			return match[1] + ":" + scopeCodexAccountIdentityValue(account, apiKeyID, "thread", match[2]), true
		}
	}
	return "", false
}

func deriveCodexConvergenceIdentityValue(account *Account, seed, raw string) (string, bool) {
	if !codexIdentityV2Enabled(account) {
		return "", false
	}
	parsed, err := uuid.Parse(raw)
	if err != nil || parsed.Version() != 7 || parsed.String() != raw {
		return "", false
	}
	hash := sha256.Sum256([]byte(seed))
	var derived uuid.UUID
	copy(derived[:], hash[:16])
	copy(derived[:6], parsed[:6])
	derived[6] = (derived[6] & 0x0f) | 0x70
	derived[8] = (derived[8] & 0x3f) | 0x80
	return derived.String(), true
}

func preserveCodexConvergenceRootTurn(metadata map[string]any, ids *codexFingerprintIDs) {
	if metadata == nil || ids == nil || !ids.identityV2 || ids.turnID == "" {
		return
	}
	turn, _ := metadata["turn_id"].(string)
	root, _ := metadata["root_turn_id"].(string)
	if strings.TrimSpace(turn) != "" && root == turn {
		metadata["root_turn_id"] = ids.turnID
	}
}

var codexConvergenceBodyToHeader = [][2]string{
	{"session_id", "session-id"},
	{"session-id", "session-id"},
	{"thread_id", "thread-id"},
	{"thread-id", "thread-id"},
	{"x-codex-parent-thread-id", "x-codex-parent-thread-id"},
	{"parent_thread_id", "x-codex-parent-thread-id"},
	{"x-openai-subagent", "x-openai-subagent"},
}

const codexConvergenceStagedBodyIdentityContextKey = "codex_convergence_body_identity"

type codexConvergenceStagedBodyIdentity struct {
	accountID int64
	namespace string
	headers   map[string]string
}

// The body must already have account identity and fingerprint rewrites applied.
// Both metadata locations are accepted; no request body is decoded in full.
func stageCodexConvergenceBodyIdentityMap(c *gin.Context, source *Account, body map[string]any) {
	staged := map[string]string{}
	if codexIdentityV2Enabled(source) {
		fillCodexConvergenceIdentityMap(staged, body["client_metadata"])
		if response, ok := body["response"].(map[string]any); ok {
			fillCodexConvergenceIdentityMap(staged, response["client_metadata"])
		}
	}
	stageCodexConvergenceBodyIdentity(c, source, staged)
}

func fillCodexConvergenceIdentityMap(staged map[string]string, value any) {
	var embedded string
	for _, pair := range codexConvergenceBodyToHeader {
		var text string
		switch metadata := value.(type) {
		case map[string]any:
			text, _ = metadata[pair[0]].(string)
			embedded, _ = metadata[openAIWSTurnMetadataHeader].(string)
		case map[string]string:
			text = metadata[pair[0]]
			embedded = metadata[openAIWSTurnMetadataHeader]
		}
		if staged[pair[1]] == "" {
			setCodexConvergenceStagedValue(staged, pair[1], text)
		}
	}
	fillCodexConvergenceIdentityFrom(staged, gjson.Parse(embedded))
}

func stageCodexConvergenceBodyIdentityRaw(c *gin.Context, source *Account, body []byte) {
	staged := map[string]string{}
	if codexIdentityV2Enabled(source) {
		root := gjson.ParseBytes(body)
		for _, path := range []string{"client_metadata", "response.client_metadata"} {
			metadata := root.Get(path)
			fillCodexConvergenceIdentityFrom(staged, metadata)
			fillCodexConvergenceIdentityFrom(staged, codexConvergenceEmbeddedMetadata(metadata))
		}
	}
	stageCodexConvergenceBodyIdentity(c, source, staged)
}

func codexConvergenceMetadataString(metadata gjson.Result, name string) string {
	if !metadata.IsObject() {
		return ""
	}
	value := metadata.Get(name)
	if value.Type != gjson.String {
		return ""
	}
	return strings.TrimSpace(value.Str)
}

func codexConvergenceEmbeddedMetadata(metadata gjson.Result) gjson.Result {
	return gjson.Parse(codexConvergenceMetadataString(metadata, openAIWSTurnMetadataHeader))
}

func fillCodexConvergenceIdentityFrom(staged map[string]string, metadata gjson.Result) {
	for _, pair := range codexConvergenceBodyToHeader {
		if staged[pair[1]] == "" {
			setCodexConvergenceStagedValue(staged, pair[1], codexConvergenceMetadataString(metadata, pair[0]))
		}
	}
}

func setCodexConvergenceStagedValue(staged map[string]string, name, value string) {
	if value = strings.TrimSpace(value); value != "" {
		staged[name] = value
	}
}

func stageCodexConvergenceBodyIdentity(c *gin.Context, source *Account, staged map[string]string) {
	if c == nil {
		return
	}
	// Empty/off attempts must overwrite the previous attempt, including same-row
	// credential replacement. A prompt cache key alone is never session evidence.
	if !codexIdentityV2Enabled(source) || len(staged) == 0 || codexAccountIdentityNamespace(source) == "" {
		c.Set(codexConvergenceStagedBodyIdentityContextKey, nil)
		return
	}
	c.Set(codexConvergenceStagedBodyIdentityContextKey, codexConvergenceStagedBodyIdentity{
		accountID: source.ID,
		namespace: codexAccountIdentityNamespace(source),
		headers:   staged,
	})
}

func stagedCodexConvergenceBodyIdentity(c *gin.Context, source *Account) map[string]string {
	if c == nil || !codexIdentityV2Enabled(source) {
		return nil
	}
	value, _ := c.Get(codexConvergenceStagedBodyIdentityContextKey)
	staged, ok := value.(codexConvergenceStagedBodyIdentity)
	if !ok || staged.accountID != source.ID || staged.namespace != codexAccountIdentityNamespace(source) {
		return nil
	}
	return staged.headers
}

// Run after applyCodexAccountIdentityHeaders and applyStagedCodexFingerprintHeaders.
// Staged body and outbound metadata are already derived; inbound header evidence
// is derived exactly once. The body takes precedence to avoid split identities.
func applyCodexFingerprintConvergenceHeaders(c *gin.Context, source *Account, headers http.Header) {
	if headers == nil || !codexIdentityV2Enabled(source) || codexAccountIdentityNamespace(source) == "" {
		return
	}
	if c != nil {
		if skip, _ := c.Get("codex_wire_skip"); skip == true {
			stageCodexConvergenceBodyIdentity(c, nil, nil)
			return
		}
	}
	staged := stagedCodexConvergenceBodyIdentity(c, source)
	fromTurn := map[string]string{}
	fillCodexConvergenceIdentityFrom(fromTurn, gjson.Parse(headers.Get(openAIWSTurnMetadataHeader)))
	var inbound http.Header
	if c != nil && c.Request != nil {
		inbound = c.Request.Header
	}
	for _, field := range []struct{ name, kind string }{
		{"session-id", "session"},
		{"thread-id", "thread"},
		{"x-codex-parent-thread-id", "thread"},
		{"x-openai-subagent", ""},
	} {
		if value := staged[field.name]; value != "" {
			headers.Set(field.name, value)
			continue
		}
		if headers.Get(field.name) != "" {
			continue
		}
		if value := fromTurn[field.name]; value != "" {
			headers.Set(field.name, value)
			continue
		}
		raw := strings.TrimSpace(inbound.Get(field.name))
		if field.name == "session-id" && raw == "" {
			raw = extractClientSessionID(inbound)
		}
		if raw == "" {
			continue
		}
		if field.kind != "" {
			raw = scopeCodexAccountIdentityValue(source, getAPIKeyIDFromContext(c), field.kind, raw)
		}
		headers.Set(field.name, raw)
	}
	if thread := strings.TrimSpace(headers.Get("thread-id")); thread != "" {
		headers.Set("x-client-request-id", thread)
	}
	if strings.TrimSpace(headers.Get("session-id")) != "" {
		headers.Del("session_id")
		headers.Del("conversation_id")
	}
}
