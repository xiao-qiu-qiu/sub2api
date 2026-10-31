package service

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	_ "time/tzdata" // 精简镜像也能解析手工配置的 IANA 时区。

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	codexWireTimezoneExtraKey     = "codex_wire_timezone"
	codexWireUserLocationExtraKey = "codex_wire_user_location"
	codexEnvironmentContentKind   = "environments.environment_context"
	codexWireTimezoneDateLayout   = "2006-01-02"
	// 超出 YYYY-MM-DD 可表示范围的时间不能作为消息创建时间。
	codexEnvironmentMaxUnixSeconds = 253402300800
)

var (
	codexWireTimezoneNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+/-]{0,63}$`)
	codexEnvTimezonePattern      = regexp.MustCompile(`<timezone>([^<]*)</timezone>`)
	codexEnvCurrentDatePattern   = regexp.MustCompile(`<current_date>([^<]*)</current_date>`)
)

// applyCodexOptionalBodyFeatures 只投影显式启用的可选内容；调用方负责 HTTP 接线。
// /responses/compact 可以携带环境，但不能添加 Guardian client_metadata。
func applyCodexOptionalBodyFeatures(c *gin.Context, account *Account, targetURL string, body []byte) []byte {
	target, err := url.Parse(targetURL)
	if err != nil {
		return body
	}
	path := strings.TrimRight(target.Path, "/")
	if strings.HasSuffix(path, "/alpha/search") {
		if !codexWireOptionEnabled(c, account, "web_search_location") || !gjson.ValidBytes(body) {
			return body
		}
		return rewriteCodexOptionalUserLocation(codexAccountIdentitySource(c, account), body, true)
	}
	compact := strings.HasSuffix(path, "/responses/compact")
	if !compact && !strings.HasSuffix(path, "/responses") {
		return body
	}
	return applyCodexOptionalResponseFeatures(c, account, body, !compact)
}

// applyCodexOptionalWSFeatures 只处理已知的 response.create 帧，包括预热帧。
func applyCodexOptionalWSFeatures(c *gin.Context, account *Account, payload []byte) []byte {
	if !codexOptionalFeaturesEnabled(c, account) || !gjson.ValidBytes(payload) {
		return payload
	}
	if kind := codexEnvironmentField(gjson.ParseBytes(payload), "type"); kind.Type != gjson.String || kind.Str != "response.create" {
		return payload
	}
	return applyCodexOptionalResponseFeatures(c, account, payload, true)
}

func codexOptionalFeaturesEnabled(c *gin.Context, account *Account) bool {
	return codexWireOptionEnabled(c, account, "environment_timezone") ||
		codexWireOptionEnabled(c, account, "web_search_location") ||
		codexWireOptionEnabled(c, account, "guardian_credits")
}

func applyCodexOptionalResponseFeatures(c *gin.Context, account *Account, body []byte, guardian bool) []byte {
	if !codexOptionalFeaturesEnabled(c, account) || !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		return body
	}
	source := codexAccountIdentitySource(c, account)
	if codexWireOptionEnabled(c, account, "environment_timezone") {
		body = rewriteCodexEnvironmentTimezoneWithName(codexWireTimezoneName(source), body)
	}
	if codexWireOptionEnabled(c, account, "web_search_location") {
		body = rewriteCodexOptionalUserLocation(source, body, false)
	}
	if guardian && codexWireOptionEnabled(c, account, "guardian_credits") {
		body = applyCodexOptionalGuardianCredits(c, source, body)
	}
	return body
}

// 配置只来自凭据账号的手工值，不探测 IP、不刷新数据库、不读取历史解析值。
func codexWireTimezoneName(account *Account) string {
	if account == nil {
		return ""
	}
	name, _ := account.Extra[codexWireTimezoneExtraKey].(string)
	name = strings.TrimSpace(name)
	if _, err := codexWireTimezoneLocation(name); err != nil {
		return ""
	}
	return name
}

func codexWireTimezoneLocation(name string) (*time.Location, error) {
	if !codexWireTimezoneNamePattern.MatchString(name) || name == "Local" {
		return nil, errors.New("invalid codex wire IANA timezone")
	}
	return time.LoadLocation(name)
}

// 从 klno 提取：只有一次性 create_time 和逐项分类共同确认的 user/message
// 环境内容才能改写。普通用户文本、工具输出和缺失/重复的证据保持原始字节。
func rewriteCodexEnvironmentTimezoneWithName(name string, body []byte) []byte {
	name = strings.TrimSpace(name)
	loc, err := codexWireTimezoneLocation(name)
	if err != nil || !gjson.ValidBytes(body) {
		return body
	}
	input := codexEnvironmentField(gjson.ParseBytes(body), "input")
	if !input.IsArray() {
		return body
	}
	var rewritten []byte
	offset := 0
	input.ForEach(func(_, item gjson.Result) bool {
		if codexEnvironmentField(item, "type").Str != "message" || codexEnvironmentField(item, "role").Str != "user" {
			return true
		}
		metadata := codexEnvironmentField(item, "internal_chat_message_metadata_passthrough")
		stamp := codexEnvironmentField(metadata, "create_time")
		if stamp.Type != gjson.Number || stamp.Num < 0 || stamp.Num >= codexEnvironmentMaxUnixSeconds {
			return true
		}
		content := codexEnvironmentField(item, "content")
		kinds := codexEnvironmentField(metadata, "content_item_kinds")
		if !content.IsArray() || !kinds.IsArray() {
			return true
		}
		entries, classifications := content.Array(), kinds.Array()
		if len(entries) != len(classifications) {
			return true
		}
		createdAt := time.Unix(int64(stamp.Num), 0)
		for i, entry := range entries {
			if classifications[i].Type != gjson.String || classifications[i].Str != codexEnvironmentContentKind ||
				codexEnvironmentField(entry, "type").Str != "input_text" {
				continue
			}
			text := codexEnvironmentField(entry, "text")
			if text.Type != gjson.String {
				continue
			}
			next := rewriteCodexEnvironmentText(text, name, loc, createdAt)
			if next == text.Raw {
				continue
			}
			rewritten = append(rewritten, body[offset:text.Index]...)
			rewritten = append(rewritten, next...)
			offset = text.Index + len(text.Raw)
		}
		return true
	})
	if offset == 0 {
		return body
	}
	return append(rewritten, body[offset:]...)
}

// 不采用重复键的首值或末值猜测分类和时间；地理/metadata 改写也共用此规则。
func codexEnvironmentField(object gjson.Result, name string) gjson.Result {
	if !object.IsObject() {
		return gjson.Result{}
	}
	var result gjson.Result
	found := false
	object.ForEach(func(key, value gjson.Result) bool {
		if key.Str == name {
			if found {
				result = gjson.Result{}
				return false
			}
			result, found = value, true
		}
		return true
	})
	return result
}

func rewriteCodexEnvironmentText(text gjson.Result, name string, loc *time.Location, createdAt time.Time) string {
	block := strings.TrimSpace(text.Str)
	if !strings.HasPrefix(block, "<environment_context>") || !strings.HasSuffix(block, "</environment_context>") ||
		strings.Count(block, "<environment_context>") != 1 || strings.Count(block, "</environment_context>") != 1 {
		return text.Raw
	}
	zones := codexEnvTimezonePattern.FindAllStringSubmatch(text.Raw, 2)
	dates := codexEnvCurrentDatePattern.FindAllStringSubmatch(text.Raw, 2)
	if len(zones) != 1 || len(dates) > 1 ||
		strings.Count(block, "<timezone>") != 1 || strings.Count(block, "</timezone>") != 1 ||
		strings.Count(block, "<current_date>") != len(dates) || strings.Count(block, "</current_date>") != len(dates) ||
		!codexEnvironmentTimeFieldsAreTopLevel(block) {
		return text.Raw
	}
	source, err := codexWireTimezoneLocation(zones[0][1])
	if err != nil {
		return text.Raw
	}
	if len(dates) == 1 && strings.TrimSpace(dates[0][1]) != createdAt.In(source).Format(codexWireTimezoneDateLayout) {
		return text.Raw
	}
	next := text.Raw
	if len(dates) == 1 {
		date := createdAt.In(loc).Format(codexWireTimezoneDateLayout)
		if len(date) != len(codexWireTimezoneDateLayout) {
			return text.Raw
		}
		next = codexEnvCurrentDatePattern.ReplaceAllString(next, "<current_date>"+date+"</current_date>")
	}
	return codexEnvTimezonePattern.ReplaceAllString(next, "<timezone>"+name+"</timezone>")
}

// 标签必须是环境块的直接子项，不能把 cwd、shell 或路径里的同名片段当字段。
// XML 只用于验证结构；替换仍在原始 JSON 字符串上完成，不重新编码路径/shell。
func codexEnvironmentTimeFieldsAreTopLevel(block string) bool {
	decoder := xml.NewDecoder(strings.NewReader(block))
	depth := 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return depth == 0
		}
		if err != nil {
			return false
		}
		switch token := token.(type) {
		case xml.StartElement:
			if token.Name.Local == "timezone" || token.Name.Local == "current_date" {
				if depth != 1 || token.Name.Space != "" || len(token.Attr) != 0 {
					return false
				}
			}
			depth++
		case xml.EndElement:
			depth--
		}
	}
}

type codexOptionalUserLocation struct {
	Type     string `json:"type"`
	Country  string `json:"country"`
	Region   string `json:"region"`
	City     string `json:"city"`
	Timezone string `json:"timezone"`
}

func codexConfiguredUserLocation(account *Account) (codexOptionalUserLocation, bool) {
	if account == nil {
		return codexOptionalUserLocation{}, false
	}
	configured, ok := account.Extra[codexWireUserLocationExtraKey].(map[string]any)
	if !ok {
		return codexOptionalUserLocation{}, false
	}
	field := func(key string) string {
		value, _ := configured[key].(string)
		return strings.TrimSpace(value)
	}
	location := codexOptionalUserLocation{
		Type: "approximate", Country: field("country"), Region: field("region"),
		City: field("city"), Timezone: field("timezone"),
	}
	if location.City == "" || location.Region == "" || location.Country == "" {
		return codexOptionalUserLocation{}, false
	}
	if _, err := codexWireTimezoneLocation(location.Timezone); err != nil {
		return codexOptionalUserLocation{}, false
	}
	return location, true
}

// 仅覆盖已提供的非 null location。目标没有显式地理配置时，保留用户原位置。
// gjson 的原始偏移让非目标字段和格式保持原样；重复容器/叶子键不做推测。
func rewriteCodexOptionalUserLocation(account *Account, body []byte, alpha bool) []byte {
	location, ok := codexConfiguredUserLocation(account)
	if !ok {
		return body
	}
	root := gjson.ParseBytes(body)
	var values []gjson.Result
	if alpha {
		settings := codexEnvironmentField(root, "settings")
		value := codexEnvironmentField(settings, "user_location")
		if value.Exists() && value.Type != gjson.Null {
			values = append(values, value)
		}
	} else {
		tools := codexEnvironmentField(root, "tools")
		if !tools.IsArray() {
			return body
		}
		tools.ForEach(func(_, tool gjson.Result) bool {
			kind := codexEnvironmentField(tool, "type")
			if kind.Type != gjson.String {
				return true
			}
			// 与本地 basispoints/route.go 的已知 Responses 工具列表一致。
			switch kind.Str {
			case "web_search", "web_search_preview", "web_search_preview_2025_03_11", "web_search_2025_08_26":
				value := codexEnvironmentField(tool, "user_location")
				if value.Exists() && value.Type != gjson.Null {
					values = append(values, value)
				}
			}
			return true
		})
	}
	if len(values) == 0 {
		return body
	}
	var raw bytes.Buffer
	encoder := json.NewEncoder(&raw)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(location); err != nil {
		return body
	}
	replacement := bytes.TrimSuffix(raw.Bytes(), []byte("\n"))
	var rewritten []byte
	offset := 0
	for _, value := range values {
		rewritten = append(rewritten, body[offset:value.Index]...)
		rewritten = append(rewritten, replacement...)
		offset = value.Index + len(value.Raw)
	}
	return append(rewritten, body[offset:]...)
}
