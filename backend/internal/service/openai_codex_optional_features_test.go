package service

import (
	"fmt"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func optionalCodexTestAccount(options ...string) *Account {
	flags := make(map[string]any)
	for _, option := range options {
		flags[option] = true
	}
	return &Account{
		Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{},
		Extra: map[string]any{
			"codex_wire_options": flags, "codex_wire_timezone": "America/Los_Angeles",
			"codex_wire_user_location": map[string]any{
				"city": "Las Vegas", "region": "Nevada", "country": "US", "timezone": "America/Los_Angeles",
			},
		},
	}
}

func optionalCodexTestContext() *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	return c
}

func optionalCodexTestEnv(date, timezone string) string {
	return "<environment_context>\n<cwd>C:\\work\\proj</cwd>\n<shell>powershell</shell>\n" +
		"<current_date>" + date + "</current_date>\n<timezone>" + timezone + "</timezone>\n</environment_context>"
}

func optionalCodexTestMessage(text, stamp string, classified bool) string {
	metadata := ""
	if classified {
		metadata = `,"internal_chat_message_metadata_passthrough":{"create_time":` + stamp +
			`,"content_item_kinds":["environments.environment_context"]}`
	}
	return `{"type":"message","role":"user","content":[{"type":"input_text","text":` + strconv.Quote(text) + `}]` + metadata + `}`
}

func TestCodexOptionalEnvironmentUsesEachMessageCreationTime(t *testing.T) {
	stamp := strconv.FormatInt(time.Date(2026, 3, 1, 3, 30, 0, 0, time.UTC).Unix(), 10)
	summer := strconv.FormatInt(time.Date(2026, 7, 1, 23, 30, 0, 0, time.UTC).Unix(), 10)
	env := optionalCodexTestEnv("2026-03-01", "Asia/Shanghai")
	ordinary := optionalCodexTestMessage(env, stamp, false)
	tool := `{"type":"function_call_output","output":` + strconv.Quote(env) + `}`
	makeBody := func(first, second string) []byte {
		return []byte(`{ "keep":1e+03, "input":[` + first + `,` + ordinary + `,` + tool + `,` + second + `] }`)
	}
	body := makeBody(optionalCodexTestMessage(env, stamp, true),
		optionalCodexTestMessage(optionalCodexTestEnv("2026-07-02", "Asia/Shanghai"), summer, true))
	want := makeBody(optionalCodexTestMessage(optionalCodexTestEnv("2026-02-28", "America/Los_Angeles"), stamp, true),
		optionalCodexTestMessage(optionalCodexTestEnv("2026-07-01", "America/Los_Angeles"), summer, true))
	got := applyCodexOptionalBodyFeatures(nil, optionalCodexTestAccount("environment_timezone"), "/v1/responses", body)
	if string(got) != string(want) {
		t.Fatalf("只应改可信环境的两个字段，且保留其余字节\ngot: %s\nwant: %s", got, want)
	}
	if again := applyCodexOptionalBodyFeatures(nil, optionalCodexTestAccount("environment_timezone"), "/v1/responses", got); string(again) != string(got) {
		t.Fatal("同一历史消息重试不能改变日期")
	}
}

func TestCodexOptionalEnvironmentRequiresUnambiguousEvidence(t *testing.T) {
	const stamp = "1772335800" // 2026-03-01 03:30 UTC
	message := optionalCodexTestMessage(optionalCodexTestEnv("2026-03-01", "Asia/Shanghai"), stamp, true)
	cases := map[string]string{
		"no_metadata":         optionalCodexTestMessage(optionalCodexTestEnv("2026-03-01", "Asia/Shanghai"), stamp, false),
		"wrong_kind":          strings.Replace(message, "environments.environment_context", "user.text", 1),
		"unaligned_kinds":     strings.Replace(message, `["environments.environment_context"]`, `[]`, 1),
		"missing_timestamp":   strings.Replace(message, `"create_time":`+stamp+`,`, "", 1),
		"duplicate_timestamp": strings.Replace(message, `"create_time":`+stamp, `"create_time":0,"create_time":`+stamp, 1),
		"date_mismatch":       strings.Replace(message, "2026-03-01", "2026-03-02", 1),
		"timezone_in_shell":   optionalCodexTestMessage("<environment_context><shell>echo <timezone>Asia/Shanghai</timezone></shell></environment_context>", stamp, true),
	}
	for name, entry := range cases {
		t.Run(name, func(t *testing.T) {
			body := []byte(`{"input":[` + entry + `]}`)
			got := applyCodexOptionalBodyFeatures(nil, optionalCodexTestAccount("environment_timezone"), "/responses", body)
			if string(got) != string(body) {
				t.Fatalf("证据不足应保持原样: %s", got)
			}
		})
	}
}

func TestCodexOptionalFeaturesAreIndependentAndFollowCredentialSource(t *testing.T) {
	body := []byte(`{"input":[` + optionalCodexTestMessage(optionalCodexTestEnv("2026-03-01", "Asia/Shanghai"), "1772335800", true) +
		`],"tools":[{"type":"web_search","user_location":{"city":"Shanghai"}}]}`)
	if got := applyCodexOptionalBodyFeatures(nil, optionalCodexTestAccount(), "/responses", body); string(got) != string(body) {
		t.Fatal("默认必须全部关闭")
	}
	for _, option := range []string{"environment_timezone", "web_search_location", "guardian_credits"} {
		t.Run(option, func(t *testing.T) {
			account := optionalCodexTestAccount(option)
			c := optionalCodexTestContext()
			c.Set("openai_codex_account_identity_source", account)
			// 转发行自身没有选项或配置，必须读已解析的凭据 source。
			row := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
			got := applyCodexOptionalBodyFeatures(c, row, "/responses", body)
			if strings.Contains(gjson.GetBytes(got, "input.0.content.0.text").Str, "America/Los_Angeles") != (option == "environment_timezone") {
				t.Fatal("environment_timezone 受其它选项影响")
			}
			if (gjson.GetBytes(got, "tools.0.user_location.city").Str == "Las Vegas") != (option == "web_search_location") {
				t.Fatal("web_search_location 受其它选项影响")
			}
			marker := gjson.GetBytes(got, "client_metadata.guardian_credits_requested")
			if marker.Exists() != (option == "guardian_credits") || (marker.Exists() && (marker.Type != gjson.String || marker.Str != "true")) {
				t.Fatal("guardian_credits 必须独立控制字符串标记")
			}
		})
	}
}

func TestCodexOptionalLocationOnlyRewritesKnownProvidedLocations(t *testing.T) {
	body := []byte(`{"tools":[` +
		`{"type":"function","user_location":{"city":"keep"}},` +
		`{"type":"web_search","user_location":{"city":"Shanghai"}},` +
		`{"type":"web_search_preview","user_location":null},` +
		`{"type":"web_search_preview_2025_03_11"},` +
		`{"type":"web_search_custom","user_location":{"city":"keep"}},` +
		`{"type":"web_search_2025_08_26","user_location":{"city":"Shanghai"}}]}`)
	account := optionalCodexTestAccount("web_search_location")
	got := applyCodexOptionalBodyFeatures(nil, account, "/responses", body)
	for _, i := range []int{1, 5} {
		if city := gjson.GetBytes(got, fmt.Sprintf("tools.%d.user_location.city", i)).Str; city != "Las Vegas" {
			t.Fatalf("已知工具 %d 未改写: %s", i, got)
		}
	}
	for _, i := range []int{0, 2, 3, 4} {
		path := fmt.Sprintf("tools.%d", i)
		if gjson.GetBytes(got, path).Raw != gjson.GetBytes(body, path).Raw {
			t.Fatalf("不应修改/补齐工具 %d", i)
		}
	}
	for _, configured := range []any{nil, map[string]any{"city": "Las Vegas"}, map[string]any{"city": "Las Vegas", "region": "Nevada", "country": "US", "timezone": "Mars/Olympus"}} {
		account.Extra["codex_wire_user_location"] = configured
		if got := applyCodexOptionalBodyFeatures(nil, account, "/responses", body); string(got) != string(body) {
			t.Fatal("没有完整显式目标位置时不能删除或改写用户位置")
		}
	}
	alpha := []byte(`{"settings":{"user_location":{"city":"Shanghai"}},"tools":[{"type":"web_search","user_location":{"city":"keep"}}]}`)
	got = applyCodexOptionalBodyFeatures(nil, optionalCodexTestAccount("web_search_location", "guardian_credits"), "https://chatgpt.com/backend-api/codex/alpha/search?x=1", alpha)
	if gjson.GetBytes(got, "settings.user_location.city").Str != "Las Vegas" ||
		gjson.GetBytes(got, "tools.0.user_location.city").Str != "keep" ||
		gjson.GetBytes(got, "client_metadata").Exists() {
		t.Fatalf("alpha/search 只改 settings 位置: %s", got)
	}
}

func TestCodexOptionalGuardianExcludesCredentialKindsAndSelfRequests(t *testing.T) {
	for _, tc := range []struct {
		name, accountType, mode, body, header, guardianHeader string
		want                                                  bool
	}{
		{name: "oauth", accountType: AccountTypeOAuth, body: `{"client_metadata":{"keep":"yes"}}`, want: true},
		{name: "setup_token", accountType: AccountTypeSetupToken, body: `{}`, want: true},
		{name: "api_key", accountType: AccountTypeAPIKey, body: `{}`},
		{name: "pat", accountType: AccountTypeOAuth, mode: "personal_access_token", body: `{}`},
		{name: "agent", accountType: AccountTypeOAuth, mode: "agentIdentity", body: `{}`},
		{name: "guardian_body", accountType: AccountTypeOAuth, body: `{"client_metadata":{"x-openai-subagent":" Guardian "}}`},
		{name: "guardian_header", accountType: AccountTypeOAuth, body: `{}`, header: "guardian"},
		{name: "guardian_review_header", accountType: AccountTypeOAuth, body: `{}`, guardianHeader: "true"},
		{name: "scalar_metadata", accountType: AccountTypeOAuth, body: `{"client_metadata":"keep"}`},
		{name: "null_metadata", accountType: AccountTypeOAuth, body: `{"client_metadata":null}`},
		{name: "duplicate_metadata", accountType: AccountTypeOAuth, body: `{"client_metadata":{},"client_metadata":{"x-openai-subagent":"guardian"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := optionalCodexTestAccount("guardian_credits")
			account.Type = tc.accountType
			account.Credentials["auth_mode"] = tc.mode
			c := optionalCodexTestContext()
			c.Request.Header.Set("x-openai-subagent", tc.header)
			c.Request.Header.Set("x-codex-guardian", tc.guardianHeader)
			got := applyCodexOptionalBodyFeatures(c, account, "/responses", []byte(tc.body))
			marker := gjson.GetBytes(got, "client_metadata.guardian_credits_requested")
			if marker.Exists() != tc.want || (tc.want && (marker.Type != gjson.String || marker.Str != "true")) {
				t.Fatalf("不符合账号/评审来源条件: %s", got)
			}
			if !tc.want && string(got) != tc.body {
				t.Fatal("排除分支必须保持原始字节")
			}
		})
	}
}

func TestCodexOptionalEndpointsWSAndInvalidJSON(t *testing.T) {
	account := optionalCodexTestAccount("guardian_credits")
	for _, path := range []string{"/responses/compact", "/chat/completions", "/responses-extra"} {
		if got := applyCodexOptionalBodyFeatures(nil, account, path, []byte(`{}`)); string(got) != `{}` {
			t.Fatalf("%s 不应补 guardian 标记", path)
		}
	}
	frame := []byte(`{"type":"response.create","generate":false}`)
	got := applyCodexOptionalWSFeatures(nil, account, frame)
	if gjson.GetBytes(got, "client_metadata.guardian_credits_requested").Str != "true" {
		t.Fatal("WS 预热 response.create 也应支持 guardian 标记")
	}
	for _, payload := range []string{`{"type":"response.cancel"}`, `{"type":"response.create","type":"response.cancel"}`, `{"type":"response.create",`, `null`, `[]`} {
		if got := applyCodexOptionalWSFeatures(nil, account, []byte(payload)); string(got) != payload {
			t.Fatalf("未知帧/无效 JSON 应原样保留: %s", got)
		}
		if got := applyCodexOptionalBodyFeatures(nil, account, "/responses", []byte(payload)); !gjson.Valid(payload) && string(got) != payload {
			t.Fatal("无效 HTTP JSON 应原样保留")
		}
	}
}

func TestCodexOptionalWSProjectsNestedAndRootWithoutLosingRawValues(t *testing.T) {
	fields := `"input":[` + optionalCodexTestMessage(optionalCodexTestEnv("2026-03-01", "Asia/Shanghai"), "1772335800", true) +
		`],"tools":[{"type":"web_search","user_location":{"city":"Shanghai"}}]`
	metadata := `"client_metadata":{"sequence":9007199254740993,"opaque":{"n":1e+03}}`
	for _, withRoot := range []bool{false, true} {
		t.Run(fmt.Sprintf("root_input_tools=%t", withRoot), func(t *testing.T) {
			rootFields := ""
			if withRoot {
				rootFields = fields + ","
			}
			body := []byte(`{ "type":"response.create", "unknown":1e+03, ` + rootFields + metadata +
				`,"response":{ "unknown":9007199254740993, ` + fields + `,` + metadata + ` } }`)
			if got := applyCodexOptionalWSFeatures(nil, optionalCodexTestAccount(), body); string(got) != string(body) {
				t.Fatal("关闭开关时两层都必须字节不变")
			}
			account := optionalCodexTestAccount("environment_timezone", "web_search_location", "guardian_credits")
			got := applyCodexOptionalWSFeatures(nil, account, body)
			prefixes := []string{"response."}
			if withRoot {
				prefixes = append(prefixes, "")
			} else if gjson.GetBytes(got, "client_metadata").Raw != gjson.GetBytes(body, "client_metadata").Raw {
				t.Fatal("nested-only 不应改顶层 transport metadata 或注入 credits")
			}
			for _, prefix := range prefixes {
				if text := gjson.GetBytes(got, prefix+"input.0.content.0.text").Str; text != optionalCodexTestEnv("2026-02-28", "America/Los_Angeles") {
					t.Fatalf("%s 环境未按创建时间投影: %s", prefix, got)
				}
				if gjson.GetBytes(got, prefix+"tools.0.user_location.city").Str != "Las Vegas" {
					t.Fatalf("%s 工具位置未投影", prefix)
				}
				if marker := gjson.GetBytes(got, prefix+"client_metadata.guardian_credits_requested"); marker.Type != gjson.String || marker.Str != "true" {
					t.Fatalf("%s 缺少字符串 credits 标记", prefix)
				}
			}
			for _, path := range []string{"unknown", "response.unknown", "client_metadata.sequence", "response.client_metadata.sequence", "client_metadata.opaque", "response.client_metadata.opaque"} {
				if gjson.GetBytes(got, path).Raw != gjson.GetBytes(body, path).Raw {
					t.Fatalf("未知字段 %s 的原始数字字节被改动", path)
				}
			}
		})
	}
}

func TestCodexOptionalWSGuardianInEitherLayerSuppressesBothCredits(t *testing.T) {
	for _, guardianInRoot := range []bool{false, true} {
		t.Run(fmt.Sprintf("guardian_in_root=%t", guardianInRoot), func(t *testing.T) {
			rootSubagent, nestedSubagent := "explorer", "guardian"
			if guardianInRoot {
				rootSubagent, nestedSubagent = nestedSubagent, rootSubagent
			}
			body := []byte(`{ "type":"response.create","input":[],"client_metadata":{"sequence":9007199254740993,"x-openai-subagent":"` + rootSubagent +
				`"},"response":{"input":[],"client_metadata":{"unknown":1e+03,"x-openai-subagent":"` + nestedSubagent + `"}} }`)
			got := applyCodexOptionalWSFeatures(nil, optionalCodexTestAccount("guardian_credits"), body)
			if string(got) != string(body) {
				t.Fatalf("任一层是 guardian 时都不应在另一层补 credits，且原值须保留: %s", got)
			}
		})
	}
}
