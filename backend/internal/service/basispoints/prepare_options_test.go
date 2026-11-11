package basispoints

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestPrepareWithCatalogProtocolOptions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		scope string
		cache *CatalogCache
		extra object
	}{
		{"no cache", "scope", nil, nil},
		{"empty scope", "", new(CatalogCache), nil},
		{"cached", "scope", new(CatalogCache), nil},
		{"tool choice none", "scope", new(CatalogCache), object{"tool_choice": "none"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, skip := range []bool{false, true} {
				source := object{"model": "gpt-6-astra", "input": "test text"}
				for key, value := range tc.extra {
					source[key] = value
				}
				raw, err := json.Marshal(source)
				require.NoError(t, err)
				wire, bridge, err := PrepareWithCatalog(raw, tc.scope, nil, tc.cache, PrepareOptions{SkipProtocolInstructions: skip})
				require.NoError(t, err)
				wantItems := 2
				if skip {
					wantItems = 1
				}
				require.Len(t, gjson.GetBytes(wire, "input").Array(), wantItems)
				reprepared, _, err := bridge.Reprepare(raw)
				require.NoError(t, err)
				require.JSONEq(t, gjson.GetBytes(wire, "input").Raw, gjson.GetBytes(reprepared, "input").Raw)
			}
		})
	}
}

func TestPrepareProtocolOptionPreservesClientInstructions(t *testing.T) {
	const protocol = "This request comes from an external Responses client. Return assistant text. Do not call Excel, Office, workbook or connector tools."
	source := object{"model": "gpt-6-astra", "instructions": "Client instructions", "input": []any{message("developer", protocol), message("user", "test text")}}
	raw, err := json.Marshal(source)
	require.NoError(t, err)
	plan, err := PrepareNativeImages(raw)
	require.NoError(t, err)
	wire, bridge, err := plan.PrepareWithCatalog("scope", nil, new(CatalogCache), PrepareOptions{SkipProtocolInstructions: true})
	require.NoError(t, err)
	input := gjson.GetBytes(wire, "input").Array()
	require.Len(t, input, 3)
	require.Equal(t, "Client instructions", input[0].Get("content.0.text").String())
	require.Equal(t, protocol, input[1].Get("content.0.text").String(), "a matching client message must not be deleted")
	require.Equal(t, "test text", input[2].Get("content.0.text").String())
	reprepared, _, err := bridge.Reprepare(raw)
	require.NoError(t, err)
	require.JSONEq(t, gjson.GetBytes(wire, "input").Raw, gjson.GetBytes(reprepared, "input").Raw)
}
