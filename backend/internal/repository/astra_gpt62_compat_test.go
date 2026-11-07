package repository

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAstraGPT62DoesNotBorrowLegacyRoute(t *testing.T) {
	for _, model := range []string{openai.GPT62AstraModel, openai.GPT62SolModel} {
		for _, unified := range []bool{false, true} {
			for _, path := range []string{"/backend-api/codex/responses", "/backend-api/codex/responses/lite"} {
				t.Run(fmt.Sprintf("%s/unified=%t%s", model, unified, path), func(t *testing.T) {
					cfg := &config.Config{}
					cfg.Gateway.CodexGatewayPin = pinConfig()
					provider := &unifiedAstraProvider{enabled: unified}
					calls := 0
					s := &astraRoutingUpstream{cfg: cfg, tickets: provider, delegate: gatewayPinDelegate{call: func(r *http.Request, proxy string, id int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
						calls++
						require.Equal(t, "native-exit", proxy)
						require.Equal(t, int64(300), id)
						require.Equal(t, "native=keep", r.Header.Get("Cookie"))
						require.Empty(t, r.Header.Get("X-Codex-Turn-State"))
						body, err := io.ReadAll(r.Body)
						require.NoError(t, err)
						require.Equal(t, model, gjson.GetBytes(body, "model").String())
						return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
					}}}
					req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://chatgpt.com"+path, strings.NewReader(fmt.Sprintf(`{"model":%q}`, model)))
					require.NoError(t, err)
					req.Header.Set("Cookie", "native=keep")
					resp, err := s.Do(req, "native-exit", 300, 1)
					require.NoError(t, err)
					require.NoError(t, resp.Body.Close())
					require.Equal(t, 1, calls)
					require.Zero(t, provider.calls, "the legacy ticket provider must not be called for GPT-6.2")
				})
			}
		}
	}
}
