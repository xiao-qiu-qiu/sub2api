package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestTerminalOpenAIImagesGIFAccessErrorMatches(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"message", `{"error":{"message":"GIF output is not enabled for this request."}}`, "GIF output is not enabled for this request."},
		{"detail", `{"detail":"GIF output is not enabled for this request."}`, "GIF output is not enabled for this request."},
		{"case whitespace and no period", `{"error":{"message":"  gif OUTPUT\t is\nnot enabled for this request  "}}`, "gif OUTPUT\t is\nnot enabled for this request"},
		{"whitespace before period", `{"detail":" GIF output is not enabled for this request . \n"}`, "GIF output is not enabled for this request ."},
		{"wrapped message", `{"error":{"message":"{\"error\":{\"message\":\"GIF output is not enabled for this request.\"}}"}}`, "GIF output is not enabled for this request."},
		{"empty message uses detail", `{"error":{"message":"  "},"detail":"GIF output is not enabled for this request."}`, "GIF output is not enabled for this request."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			got := terminalOpenAIImagesGIFAccessError(c, &OpenAIImagesRequest{OutputFormat: "gif"}, http.StatusBadRequest, nil, []byte(tc.body))
			require.NotNil(t, got)
			require.Equal(t, http.StatusBadRequest, got.StatusCode)
			require.Equal(t, tc.want, got.Message)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Equal(t, http.StatusBadRequest, c.GetInt(OpsUpstreamStatusCodeKey))
			require.Equal(t, tc.want, c.GetString(OpsUpstreamErrorMessageKey))
			require.False(t, IsOpenAIImagesRetryableUpstreamError(got))
		})
	}
}

func TestTerminalOpenAIImagesGIFAccessErrorIgnoresOtherErrors(t *testing.T) {
	const exact = `{"error":{"message":"GIF output is not enabled for this request."}}`
	for _, tc := range []struct {
		name   string
		parsed *OpenAIImagesRequest
		status int
		body   string
	}{
		{"nil request", nil, 400, exact},
		{"png", &OpenAIImagesRequest{OutputFormat: "png"}, 400, exact},
		{"unspecified format", &OpenAIImagesRequest{}, 400, exact},
		{"success", &OpenAIImagesRequest{OutputFormat: "gif"}, 200, exact},
		{"forbidden", &OpenAIImagesRequest{OutputFormat: "gif"}, 403, exact},
		{"rate limit", &OpenAIImagesRequest{OutputFormat: "gif"}, 429, exact},
		{"server error", &OpenAIImagesRequest{OutputFormat: "gif"}, 500, exact},
		{"raw text", &OpenAIImagesRequest{OutputFormat: "gif"}, 400, "GIF output is not enabled for this request."},
		{"invalid JSON", &OpenAIImagesRequest{OutputFormat: "gif"}, 400, `{"error":{"message":"GIF output is not enabled for this request."}`},
		{"top level message", &OpenAIImagesRequest{OutputFormat: "gif"}, 400, `{"message":"GIF output is not enabled for this request."}`},
		{"empty structured fields", &OpenAIImagesRequest{OutputFormat: "gif"}, 400, `{"error":{"message":""},"detail":" ","message":"GIF output is not enabled for this request."}`},
		{"echo", &OpenAIImagesRequest{OutputFormat: "gif"}, 400, `{"error":{"message":"Invalid prompt"},"prompt":"GIF output is not enabled for this request."}`},
		{"message takes precedence", &OpenAIImagesRequest{OutputFormat: "gif"}, 400, `{"error":{"message":"Invalid prompt"},"detail":"GIF output is not enabled for this request."}`},
		{"prefix", &OpenAIImagesRequest{OutputFormat: "gif"}, 400, `{"error":{"message":"Error: GIF output is not enabled for this request."}}`},
		{"suffix", &OpenAIImagesRequest{OutputFormat: "gif"}, 400, `{"error":{"message":"GIF output is not enabled for this request. Try another account."}}`},
		{"similar", &OpenAIImagesRequest{OutputFormat: "gif"}, 400, `{"error":{"message":"GIF output is not enabled for this account."}}`},
		{"extra punctuation", &OpenAIImagesRequest{OutputFormat: "gif"}, 400, `{"detail":"GIF output is not enabled for this request.."}`},
		{"object detail", &OpenAIImagesRequest{OutputFormat: "gif"}, 400, `{"detail":{"message":"GIF output is not enabled for this request."}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			got := terminalOpenAIImagesGIFAccessError(c, tc.parsed, tc.status, nil, []byte(tc.body))
			require.Nil(t, got)
			require.False(t, c.Writer.Written())
			require.Empty(t, rec.Body.String())
			_, exists := c.Get(OpsUpstreamStatusCodeKey)
			require.False(t, exists)
			_, exists = c.Get(OpsUpstreamErrorMessageKey)
			require.False(t, exists)
		})
	}
}

func TestTerminalOpenAIImagesGIFAccessErrorPreservesUpstreamFields(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"error":{"type":"invalid_request_error","code":"gif_not_enabled","message":"GIF output is not enabled for this request.","param":"output_format"}}`)
	header := make(http.Header)
	header.Set("x-request-id", "req_gif_access")

	got := terminalOpenAIImagesGIFAccessError(c, &OpenAIImagesRequest{OutputFormat: "gif"}, 400, header, body)
	require.Equal(t, &OpenAIImagesUpstreamError{
		StatusCode:        400,
		ErrorType:         "invalid_request_error",
		Code:              "gif_not_enabled",
		Message:           "GIF output is not enabled for this request.",
		Param:             "output_format",
		UpstreamRequestID: "req_gif_access",
	}, got)
	require.Equal(t, 400, rec.Code)
	require.JSONEq(t, string(body), rec.Body.String())
	require.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))
}

func TestTerminalOpenAIImagesGIFAccessErrorDoesNotOverwriteWrittenResponse(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.JSON(http.StatusAccepted, gin.H{"existing": "response"})
	before := rec.Body.String()

	got := terminalOpenAIImagesGIFAccessError(c, &OpenAIImagesRequest{OutputFormat: "gif"}, 400, nil,
		[]byte(`{"error":{"message":"GIF output is not enabled for this request."}}`))
	require.NotNil(t, got)
	require.Equal(t, 400, got.StatusCode)
	require.Equal(t, http.StatusAccepted, rec.Code)
	require.Equal(t, before, rec.Body.String())
	require.True(t, json.Valid(rec.Body.Bytes()))
	require.Equal(t, 400, c.GetInt(OpsUpstreamStatusCodeKey))
	require.Equal(t, got.Message, c.GetString(OpsUpstreamErrorMessageKey))
}
