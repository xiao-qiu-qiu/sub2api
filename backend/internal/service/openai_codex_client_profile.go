package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
)

const (
	codexClientProfileExtraKey = "codex_client_profile"
	codexDesktopOriginator     = "Codex Desktop"
	codexExecOriginator        = "codex_exec"
)

// CodexClientProfile 是账号显式选择的客户端身份，仅适用于 OpenAI OAuth / setup-token。
// 未配置或存量值非法时沿用原有账号 UA 和规范身份链，没有全局或内置 Desktop 预设。
//
// 配置字段兼容 klno，但版本来源沿用本地实现：
//   - versionMode 必须是 latest 或 fixed，versionLag 仅支持 0。
//   - latest 的 core 取 CodexCanonicalClientVersion（面板版本 / 自动同步 / 编译期兜底），
//     不接受 codexVersion，以免同一配置出现两个 core 来源。
//   - fixed 必须明确 codexVersion，并独立于规范版本链。
//   - Desktop 在两种模式下均必须明确 desktopVersion；它是应用版本，不能用 core
//     代填。本实现不下载应用制品，也不自动推断应用版本或验证 app/core 发布配对。
//
// 示例：{"client":"desktop","platform":"windows","versionMode":"latest",
// "versionLag":0,"desktopVersion":"26.1016.62342"}。
type CodexClientProfile struct {
	Client         string `json:"client"`
	Platform       string `json:"platform"`
	CLIEntry       string `json:"cliEntry,omitempty"`
	VersionMode    string `json:"versionMode"`
	VersionLag     int    `json:"versionLag,omitempty"`
	OSType         string `json:"osType,omitempty"`
	OSVersion      string `json:"osVersion,omitempty"`
	Arch           string `json:"arch,omitempty"`
	Terminal       string `json:"terminal,omitempty"`
	CodexVersion   string `json:"codexVersion,omitempty"`
	DesktopVersion string `json:"desktopVersion,omitempty"`
}

type codexPlatformDefault struct{ osType, osVersion, arch string }

// 平台模板沿用参考实现的静态值，仅用于显式 profile 中未填的字段；不参与缺省身份，
// 也不表示系统版本已同步或检测过。管理员可用 osType/osVersion/arch 覆盖。
var codexPlatformDefaults = map[string]codexPlatformDefault{
	"macos":   {"Mac OS", "26.7.1", "arm64"},
	"windows": {"Windows", "10.0.26200", "x86_64"},
	"linux":   {"Ubuntu", "26.4.0", "x86_64"},
}

// ParseCodexClientProfile 严格解码一个配置对象，支持 extra 中的 map、结构体和
// json.RawMessage；拒绝未知字段和尾随数据。清除配置的 null 由 extra 校验入口处理。
func ParseCodexClientProfile(raw any) (*CodexClientProfile, error) {
	data, ok := raw.(json.RawMessage)
	if !ok {
		var err error
		data, err = json.Marshal(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid codex client profile: %w", err)
		}
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return nil, errors.New("invalid codex client profile: expected an object")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var profile CodexClientProfile
	if err := decoder.Decode(&profile); err != nil {
		return nil, fmt.Errorf("invalid codex client profile: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid codex client profile: trailing data after the object")
	}
	if err := profile.validate(); err != nil {
		return nil, err
	}
	return &profile, nil
}

func (p *CodexClientProfile) validate() error {
	if p.Client != "desktop" && p.Client != "cli" {
		return errors.New("client must be desktop or cli")
	}
	if _, ok := codexPlatformDefaults[p.Platform]; !ok {
		return errors.New("platform must be macos, windows or linux")
	}
	if p.CLIEntry != "" && (p.CLIEntry != "exec" || p.Client != "cli") {
		return errors.New("cliEntry is only exec, and only for the cli client")
	}
	for _, field := range []struct{ name, value string }{
		{"osType", p.OSType}, {"osVersion", p.OSVersion}, {"arch", p.Arch}, {"terminal", p.Terminal},
	} {
		if field.value != "" && !codexProfileTokenOK(field.value) {
			return fmt.Errorf("%s must be 1-128 printable ASCII characters without ( ) ; \\ or edge spaces", field.name)
		}
	}
	if p.VersionLag != 0 {
		return errors.New("versionLag must be 0; version history is not available")
	}
	switch p.VersionMode {
	case "latest":
		if p.CodexVersion != "" {
			return errors.New("latest mode takes no codexVersion; core follows the canonical client version")
		}
	case "fixed":
		if !codexCoreVersionOK(p.CodexVersion) {
			return fmt.Errorf("codexVersion must be a Codex core version not below %s", codexUpstreamMinVersion)
		}
	default:
		return errors.New("versionMode must be latest or fixed")
	}
	if p.Client == "desktop" && !codexDesktopVersionOK(p.DesktopVersion) {
		return errors.New("desktopVersion is required for desktop in both latest and fixed modes and must look like 26.1016.62342")
	}
	if p.Client == "cli" && p.DesktopVersion != "" {
		return errors.New("the cli client has no desktopVersion")
	}
	return nil
}

func codexCoreVersionOK(version string) bool {
	if version == "" || NormalizeCodexClientVersion(version) != version {
		return false
	}
	// CompareVersions 不解析预发布段；先去掉该段再比较数字，避免 patch 版本的
	// 预发布（如 0.144.1-alpha.1）被误读为 patch=0。门槛自身的预发布仍不达标。
	base, _, _ := strings.Cut(version, "-")
	order := CompareVersions(base, codexUpstreamMinVersion)
	return order > 0 || order == 0 && version == codexUpstreamMinVersion
}

// 与参考实现相同，应用版本接受至少两段点分隔的纯数字，不套用 core 的版本门槛。
func codexDesktopVersionOK(version string) bool {
	if len(version) > codexClientVersionMaxLen {
		return false
	}
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if part == "" || strings.Trim(part, "0123456789") != "" {
			return false
		}
	}
	return true
}

func codexProfileTokenOK(value string) bool {
	if value == "" || len(value) > 128 || strings.TrimSpace(value) != value || strings.ContainsAny(value, "();\\") {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 32 || value[i] > 126 {
			return false
		}
	}
	return true
}

func (p *CodexClientProfile) originator() string {
	switch {
	case p.Client == "desktop":
		return codexDesktopOriginator
	case p.CLIEntry == "exec":
		return codexExecOriginator
	default:
		return openai.CodexCLIOriginator
	}
}

// identity 只接收校验后的配置。UA 首段与 version 头共用一次解析得到的 core；
// Desktop 尾组保留独立的应用版本，不经旧 UA 版本重写函数。
func (p *CodexClientProfile) identity() codexOutboundIdentity {
	core := p.CodexVersion
	if p.VersionMode == "latest" {
		core = CodexCanonicalClientVersion()
	}
	defaults := codexPlatformDefaults[p.Platform]
	originator := p.originator()
	ua := fmt.Sprintf("%s/%s (%s %s; %s) %s", originator, core,
		firstNonEmpty(p.OSType, defaults.osType), firstNonEmpty(p.OSVersion, defaults.osVersion),
		firstNonEmpty(p.Arch, defaults.arch), firstNonEmpty(p.Terminal, "unknown"))
	switch originator {
	case codexDesktopOriginator:
		ua += " (" + codexDesktopOriginator + "; " + p.DesktopVersion + ")"
	case codexExecOriginator:
		ua += " (" + codexExecOriginator + "; " + core + ")"
	}
	return codexOutboundIdentity{userAgent: ua, originator: originator, version: core}
}

// codexClientProfile 只读取显式账号配置；非法存量值回退原身份链，API Key 不启用。
func (a *Account) codexClientProfile() *CodexClientProfile {
	if !a.IsOpenAIOAuthLike() || a.Extra[codexClientProfileExtraKey] == nil {
		return nil
	}
	profile, err := ParseCodexClientProfile(a.Extra[codexClientProfileExtraKey])
	if err != nil {
		return nil
	}
	return profile
}

// validateCodexClientProfileExtra 校验管理端提交值；缺失或 null 允许保留/清除配置。
// shadow 与母账号共用上游凭据，配置必须放在母账号上，由请求阶段解析出的凭据源读取。
func validateCodexClientProfileExtra(account *Account, extra map[string]any) error {
	raw := extra[codexClientProfileExtraKey]
	if raw == nil {
		return nil
	}
	if !account.IsOpenAIOAuthLike() {
		return infraerrors.New(http.StatusBadRequest, "CODEX_CLIENT_PROFILE_INVALID", "Codex client profile requires an OpenAI OAuth or setup-token account")
	}
	if account.IsShadow() {
		return infraerrors.New(http.StatusBadRequest, "CODEX_CLIENT_PROFILE_INVALID", "set the Codex client profile on the parent account; a spark shadow uses the parent's")
	}
	if _, err := ParseCodexClientProfile(raw); err != nil {
		return infraerrors.New(http.StatusBadRequest, "CODEX_CLIENT_PROFILE_INVALID", err.Error())
	}
	return nil
}

// codexClientIdentityFor：凭据源的显式 profile → 转发行原有自定义 UA → 规范身份链。
// 原有 UA 覆写仍取转发行，保持未配 profile 时的行为。
func codexClientIdentityFor(account, credential *Account) codexOutboundIdentity {
	if profile := credential.codexClientProfile(); profile != nil {
		return profile.identity()
	}
	var overrideUA string
	if account != nil {
		overrideUA = account.GetOpenAIUserAgent()
	}
	return resolveCodexOutboundIdentity(overrideUA)
}

func enforceCodexClientIdentityHeaders(h http.Header, account, credential *Account) {
	// 保持旧收口边界：compat 桥接主动删除 originator 时，不重新补回身份。
	if h == nil || h.Get("originator") == "" {
		return
	}
	if !codexIdentityEnforcement.Load() {
		// 关闭 enforcement 时，profile 与账号 UA 都不参与；沿用最终 UA 配对逻辑。
		pairCodexIdentityHeaders(h)
		return
	}
	identity := codexClientIdentityFor(account, credential)
	// 清掉继承头中非规范大小写的同名键，避免多个身份同时出站。
	for key := range h {
		switch strings.ToLower(key) {
		case "user-agent", "originator", "version":
			delete(h, key)
		}
	}
	h.Set("user-agent", identity.userAgent)
	h.Set("originator", identity.originator)
	h.Set("version", identity.version)
}

// enforceCodexClientIdentity 供 HTTP / WS OAuth 在最终头覆写后收口。
// 调用前须按现有流程 prepareCodexAccountIdentitySource，确保 shadow / failover
// 使用本次尝试的凭据源。ForceCodexCLI 优先，继续使用原规范身份。
func (s *OpenAIGatewayService) enforceCodexClientIdentity(c *gin.Context, h http.Header, account *Account) {
	if s != nil && s.cfg != nil && s.cfg.Gateway.ForceCodexCLI {
		enforceCodexIdentityHeaders(h)
		return
	}
	enforceCodexClientIdentityHeaders(h, account, codexAccountIdentitySource(c, account))
}
