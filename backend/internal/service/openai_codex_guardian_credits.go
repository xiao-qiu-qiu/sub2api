package service

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const codexGuardianCreditsRequestedKey = "guardian_credits_requested"

// 由可选功能入口完成 guardian_credits 门控；这里仅验证凭据种类和评审来源。
// account 是本地已解析的凭据 source，不触发账号查询或 gwpool 解析。
func applyCodexOptionalGuardianCredits(c *gin.Context, account *Account, body []byte) []byte {
	if account == nil || !account.IsOpenAIOAuthLike() || account.IsOpenAIPersonalAccessToken() || account.IsOpenAIAgentIdentity() {
		return body
	}
	root := gjson.ParseBytes(body)
	metadata := codexEnvironmentField(root, "client_metadata")
	// null、标量、重复 client_metadata 都不能被替换成网关生成的对象。
	if root.Get("client_metadata").Exists() && !metadata.IsObject() {
		return body
	}
	if codexOptionalGuardianSelfRequest(c, root) {
		return body
	}
	// 重复目标键语义不确定，不能只覆盖 gjson 读到的第一份。
	if existing := metadata.Get(codexGuardianCreditsRequestedKey); existing.Exists() &&
		!codexEnvironmentField(metadata, codexGuardianCreditsRequestedKey).Exists() {
		return body
	}
	next, err := sjson.SetBytes(body, "client_metadata."+codexGuardianCreditsRequestedKey, "true")
	if err != nil {
		return body
	}
	return next
}

// WS 的 root 和 response 共用同一评审来源；检查全部 metadata，包含重复键。
func codexOptionalGuardianSelfRequest(c *gin.Context, payloads ...gjson.Result) bool {
	if c != nil && c.Request != nil {
		if strings.TrimSpace(c.GetHeader("x-codex-guardian")) != "" {
			return true
		}
		for _, subagent := range c.Request.Header.Values("x-openai-subagent") {
			if strings.EqualFold(strings.TrimSpace(subagent), "guardian") {
				return true
			}
		}
	}
	for _, payload := range payloads {
		guardian := false
		payload.ForEach(func(key, metadata gjson.Result) bool {
			if key.Str == "client_metadata" && metadata.IsObject() {
				metadata.ForEach(func(key, value gjson.Result) bool {
					if key.Str == "x-openai-subagent" && value.Type == gjson.String && strings.EqualFold(strings.TrimSpace(value.Str), "guardian") {
						guardian = true
					}
					return !guardian
				})
			}
			return !guardian
		})
		if guardian {
			return true
		}
	}
	return false
}
