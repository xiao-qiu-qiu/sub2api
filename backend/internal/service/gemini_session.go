package service

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/cespare/xxhash/v2"
	"github.com/tidwall/gjson"
)

func GenerateGeminiContentSessionHash(apiKeyID int64, model string, body []byte) string {
	root := parseRawJSONView(body)
	if !root.IsObject() {
		return ""
	}
	firstUserParts := firstGeminiUserParts(root.Get("contents"))
	if !firstUserParts.Exists() {
		return ""
	}

	digest := xxhash.New()
	_, _ = digest.WriteString("gemini_cs_key=")
	_, _ = digest.WriteString(strconv.FormatInt(apiKeyID, 10))
	if model = strings.TrimSpace(model); model != "" {
		_, _ = digest.WriteString("|model=")
		_, _ = digest.WriteString(model)
	}
	if tools := root.Get("tools"); tools.IsArray() {
		if normalized, ok := normalizeNonEmptyCompatSeedJSON(tools); ok {
			_, _ = digest.WriteString("|tools=")
			_, _ = digest.WriteString(normalized)
		}
	}
	system := root.Get("systemInstruction")
	if !system.Exists() {
		system = root.Get("system_instruction")
	}
	if normalized, ok := normalizeNonEmptyCompatSeedJSON(system); ok {
		_, _ = digest.WriteString("|system=")
		_, _ = digest.WriteString(normalized)
	}
	_, _ = digest.WriteString("|first_user=")
	_, _ = digest.WriteString(normalizeCompatSeedJSON(json.RawMessage(firstUserParts.Raw)))
	return fmt.Sprintf("%016x", digest.Sum64())
}

func firstGeminiUserParts(contents gjson.Result) gjson.Result {
	var parts gjson.Result
	if !contents.IsArray() {
		return parts
	}
	contents.ForEach(func(_, content gjson.Result) bool {
		if content.IsObject() {
			if role := content.Get("role").String(); role != "" && role != "user" {
				return true
			}
			if candidate := content.Get("parts"); hasMeaningfulGeminiParts(candidate) {
				parts = candidate
			}
		}
		return false
	})
	return parts
}

func hasMeaningfulGeminiParts(parts gjson.Result) bool {
	if !parts.IsArray() {
		return false
	}
	meaningful := false
	parts.ForEach(func(_, part gjson.Result) bool {
		if text := part.Get("text"); text.Exists() {
			meaningful = strings.TrimSpace(text.String()) != ""
		} else if part.IsObject() {
			part.ForEach(func(_, _ gjson.Result) bool {
				meaningful = true
				return false
			})
		}
		return !meaningful
	})
	return meaningful
}

// shortHash 使用 XXHash64 + Base36 生成短 hash（16 字符）
// XXHash64 比 SHA256 快约 10 倍，Base36 比 Hex 短约 20%
func shortHash(data []byte) string {
	h := xxhash.Sum64(data)
	return strconv.FormatUint(h, 36)
}

// BuildGeminiDigestChain 根据 Gemini 请求生成摘要链
// 格式: s:<hash>-u:<hash>-m:<hash>-u:<hash>-...
// s = systemInstruction, u = user, m = model
func BuildGeminiDigestChain(req *antigravity.GeminiRequest) string {
	if req == nil {
		return ""
	}

	var parts []string

	// 1. system instruction
	if req.SystemInstruction != nil && len(req.SystemInstruction.Parts) > 0 {
		partsData, _ := json.Marshal(req.SystemInstruction.Parts)
		parts = append(parts, "s:"+shortHash(partsData))
	}

	// 2. contents
	for _, c := range req.Contents {
		prefix := "u" // user
		if c.Role == "model" {
			prefix = "m"
		}
		partsData, _ := json.Marshal(c.Parts)
		parts = append(parts, prefix+":"+shortHash(partsData))
	}

	return strings.Join(parts, "-")
}

// GenerateGeminiPrefixHash 生成前缀 hash（用于分区隔离）
// 组合: userID + apiKeyID + ip + userAgent + platform + model
// 返回 16 字符的 Base64 编码的 SHA256 前缀
func GenerateGeminiPrefixHash(userID, apiKeyID int64, ip, userAgent, platform, model string) string {
	// 组合所有标识符
	normalizedUserAgent := NormalizeSessionUserAgent(userAgent)
	combined := strconv.FormatInt(userID, 10) + ":" +
		strconv.FormatInt(apiKeyID, 10) + ":" +
		ip + ":" +
		normalizedUserAgent + ":" +
		platform + ":" +
		model

	hash := sha256.Sum256([]byte(combined))
	// 取前 12 字节，Base64 编码后正好 16 字符
	return base64.RawURLEncoding.EncodeToString(hash[:12])
}

// ParseGeminiSessionValue 解析 Gemini 会话缓存值
// 格式: {uuid}:{accountID}
func ParseGeminiSessionValue(value string) (uuid string, accountID int64, ok bool) {
	if value == "" {
		return "", 0, false
	}

	// 找到最后一个 ":" 的位置（因为 uuid 可能包含 ":"）
	i := strings.LastIndex(value, ":")
	if i <= 0 || i >= len(value)-1 {
		return "", 0, false
	}

	uuid = value[:i]
	accountID, err := strconv.ParseInt(value[i+1:], 10, 64)
	if err != nil {
		return "", 0, false
	}

	return uuid, accountID, true
}

// FormatGeminiSessionValue 格式化 Gemini 会话缓存值
// 格式: {uuid}:{accountID}
func FormatGeminiSessionValue(uuid string, accountID int64) string {
	return uuid + ":" + strconv.FormatInt(accountID, 10)
}

// geminiDigestSessionKeyPrefix Gemini 摘要 fallback 会话 key 前缀
const geminiDigestSessionKeyPrefix = "gemini:digest:"

// GenerateGeminiDigestSessionKey 生成 Gemini 摘要 fallback 的 sessionKey
// 组合 prefixHash 前 8 位 + uuid 前 8 位，确保不同会话产生不同的 sessionKey
// 用于在 SelectAccountWithLoadAwareness 中保持粘性会话
func GenerateGeminiDigestSessionKey(prefixHash, uuid string) string {
	prefix := prefixHash
	if len(prefixHash) >= 8 {
		prefix = prefixHash[:8]
	}
	uuidPart := uuid
	if len(uuid) >= 8 {
		uuidPart = uuid[:8]
	}
	return geminiDigestSessionKeyPrefix + prefix + ":" + uuidPart
}
