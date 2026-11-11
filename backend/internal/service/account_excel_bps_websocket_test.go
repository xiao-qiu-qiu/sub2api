package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAccountExcelBPSTransport(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		want  string
	}{
		{"missing", nil, ExcelBPSTransportSSE},
		{"sse", "sse", ExcelBPSTransportSSE},
		{"ws", "ws", ExcelBPSTransportWS},
		{"unknown", "websocket", ExcelBPSTransportSSE},
		{"case_sensitive", "WS", ExcelBPSTransportSSE},
		{"whitespace", " ws ", ExcelBPSTransportSSE},
		{"wrong_type", true, ExcelBPSTransportSSE},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{"openai_excel_bps": true}}
			if tc.value != nil {
				a.Extra[ExcelBPSTransportKey] = tc.value
			}
			require.Equal(t, tc.want, a.ExcelBPSTransport())
		})
	}

	for _, tc := range []struct {
		name    string
		account *Account
	}{
		{"nil", nil},
		{"no_extra", &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}},
		{"disabled", &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{ExcelBPSTransportKey: "ws"}}},
		{"explicitly_disabled", &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{"openai_excel_bps": false, ExcelBPSTransportKey: "ws"}}},
		{"api_key", &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{"openai_excel_bps": true, ExcelBPSTransportKey: "ws"}}},
		{"other_platform", &Account{Platform: PlatformAnthropic, Type: AccountTypeOAuth, Extra: map[string]any{"openai_excel_bps": true, ExcelBPSTransportKey: "ws"}}},
		{"free_plan", &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"plan_type": "free"}, Extra: map[string]any{"openai_excel_bps": true, ExcelBPSTransportKey: "ws"}}},
		{"authorization_pending", &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{"openai_excel_bps": true, ExcelBPSAuthorizationPendingKey: true, ExcelBPSTransportKey: "ws"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, ExcelBPSTransportSSE, tc.account.ExcelBPSTransport())
		})
	}
}
