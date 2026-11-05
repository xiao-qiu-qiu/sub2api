package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func gifRequestTestBody(t *testing.T, fields map[string]any, multipartRequest bool) ([]byte, string) {
	t.Helper()
	if !multipartRequest {
		body, err := json.Marshal(fields)
		require.NoError(t, err)
		return body, "application/json"
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for key, value := range fields {
		require.NoError(t, w.WriteField(key, fmt.Sprint(value)))
	}
	// Edits require a source image before GIF endpoint validation can run.
	part, err := w.CreateFormFile("image", "input.png")
	require.NoError(t, err)
	_, err = part.Write([]byte("source-image"))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return body.Bytes(), w.FormDataContentType()
}

func gifResponseTestBytes(t *testing.T) []byte {
	t.Helper()
	palette := color.Palette{color.Black, color.White}
	animation := &gif.GIF{Delay: []int{20, 20, 20}, LoopCount: 0}
	for i := 0; i < 3; i++ {
		frame := image.NewPaletted(image.Rect(0, 0, 32, 16), palette)
		frame.SetColorIndex(i, 0, 1)
		animation.Image = append(animation.Image, frame)
	}
	var body bytes.Buffer
	require.NoError(t, gif.EncodeAll(&body, animation))
	return body.Bytes()
}

func TestOpenAIImagesGIFValidation(t *testing.T) {
	for _, multipartRequest := range []bool{false, true} {
		for _, tc := range []struct {
			name     string
			fields   map[string]any
			endpoint string
			wantErr  string
		}{
			{name: "defaults"},
			{name: "minimum", fields: map[string]any{"frame_count": 2, "frame_duration_ms": 20, "size": "32x16"}},
			{name: "maximum", fields: map[string]any{"frame_count": 64, "frame_duration_ms": 2000}},
			{name: "new_guide_limit", fields: map[string]any{"frame_count": 17}},
			{name: "upstream_quantization", fields: map[string]any{"frame_duration_ms": 25}},
			{name: "few_frames", fields: map[string]any{"frame_count": 1}, wantErr: "frame_count"},
			{name: "too_many_frames", fields: map[string]any{"frame_count": 65}, wantErr: "frame_count"},
			{name: "fractional_frames", fields: map[string]any{"frame_count": 3.5}, wantErr: "frame_count"},
			{name: "short_delay", fields: map[string]any{"frame_duration_ms": 19}, wantErr: "frame_duration_ms"},
			{name: "long_delay", fields: map[string]any{"frame_duration_ms": 2001}, wantErr: "frame_duration_ms"},
			{name: "fractional_delay", fields: map[string]any{"frame_duration_ms": 25.5}, wantErr: "frame_duration_ms"},
			{name: "stream", fields: map[string]any{"stream": true}, wantErr: "stream"},
			{name: "partial_zero", fields: map[string]any{"partial_images": 0}, wantErr: "partial_images"},
			{name: "compression", fields: map[string]any{"output_compression": 100}, wantErr: "output_compression"},
			{name: "mask", fields: map[string]any{"mask": ""}, wantErr: "mask"},
			{name: "rewriter_false", fields: map[string]any{"prompt_rewriter": false}, wantErr: "prompt_rewriter"},
			{name: "zero_size", fields: map[string]any{"size": "0x16"}, wantErr: "size"},
			{name: "non_multiple_size", fields: map[string]any{"size": "31x16"}, wantErr: "size"},
			{name: "old_model", fields: map[string]any{"model": "gpt-image-2.5-flare"}, wantErr: "requires gpt-image-2.6"},
			{name: "unknown_snapshot", fields: map[string]any{"model": "gpt-image-2.6-flare-2026-11-05"}, wantErr: "requires gpt-image-2.6"},
			{name: "edit", endpoint: openAIImagesEditsEndpoint, wantErr: "GIF edits"},
			{name: "frames_without_gif", fields: map[string]any{"output_format": "png", "frame_count": 3}, wantErr: "require output_format=gif"},
			{name: "delay_without_gif", fields: map[string]any{"output_format": "webp", "frame_duration_ms": 200}, wantErr: "require output_format=gif"},
		} {
			t.Run(fmt.Sprintf("%s/multipart=%t", tc.name, multipartRequest), func(t *testing.T) {
				fields := map[string]any{"model": "gpt-image-2.6-flare", "prompt": "a looping dot", "output_format": "gif"}
				for key, value := range tc.fields {
					fields[key] = value
				}
				endpoint := firstNonEmptyString(tc.endpoint, openAIImagesGenerationsEndpoint)
				if endpoint == openAIImagesEditsEndpoint && !multipartRequest {
					fields["images"] = []map[string]string{{"image_url": "data:image/png;base64,AA=="}}
				}
				body, contentType := gifRequestTestBody(t, fields, multipartRequest)
				c, _ := newOpenAIImagesTestContext(t, body)
				c.Request.URL.Path = endpoint
				c.Request.Header.Set("Content-Type", contentType)
				parsed, err := newOpenAIImagesTestService(nil).ParseOpenAIImagesRequest(c, body)
				if tc.wantErr != "" {
					require.ErrorContains(t, err, tc.wantErr)
					return
				}
				require.NoError(t, err)
				require.True(t, parsed.HasNativeOptions)
				payload, _, err := buildOpenAIImagesOAuthPayload(parsed, parsed.Model)
				require.NoError(t, err)
				for _, key := range []string{"frame_count", "frame_duration_ms"} {
					value, exists := tc.fields[key]
					require.Equal(t, exists, gjson.GetBytes(payload, key).Exists())
					if exists {
						require.EqualValues(t, value, gjson.GetBytes(payload, key).Int())
					}
				}
			})
		}
	}
	for _, field := range []string{"frame_count", "frame_duration_ms"} {
		for _, value := range []string{`"3"`, `true`, `{}`, `[]`, `-2`, `1e100`} {
			t.Run(field+"/type/"+value, func(t *testing.T) {
				body := []byte(fmt.Sprintf(`{"model":"gpt-image-2.6-flare","prompt":"loop","output_format":"gif",%q:%s}`, field, value))
				c, _ := newOpenAIImagesTestContext(t, body)
				_, err := newOpenAIImagesTestService(nil).ParseOpenAIImagesRequest(c, body)
				require.ErrorContains(t, err, field)
			})
		}
	}
}

func TestOpenAIImagesGIFForwarding(t *testing.T) {
	gifBytes := gifResponseTestBytes(t)
	encoded := base64.StdEncoding.EncodeToString(gifBytes)
	for _, model := range []string{"gpt-image-2.6-flare", "gpt-image-2.6-sunburst"} {
		for _, accountType := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
			for _, multipartRequest := range []bool{false, true} {
				for _, responseFormat := range []string{"b64_json", "url"} {
					t.Run(fmt.Sprintf("%s/%s/multipart=%t/%s", model, accountType, multipartRequest, responseFormat), func(t *testing.T) {
						body, contentType := gifRequestTestBody(t, map[string]any{
							"model": model, "prompt": "loop", "output_format": "gif", "frame_count": 3,
							"frame_duration_ms": 25, "size": "auto", "response_format": responseFormat,
						}, multipartRequest)
						c, rec := newOpenAIImagesTestContext(t, body)
						c.Request.Header.Set("Content-Type", contentType)
						responseBody := fmt.Sprintf(`{"output_format":"png","size":"auto","data":[{"b64_json":%q}],"usage":{"input_tokens":10,"output_tokens":20}}`, encoded)
						upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(responseBody))}}
						svc := newOpenAIImagesTestService(upstream)
						parsed, err := svc.ParseOpenAIImagesRequest(c, body)
						require.NoError(t, err)
						account := newOpenAIImagesAPIKeyAccount()
						wantPath := "/v1/images/generations"
						if accountType == AccountTypeOAuth {
							account = directImagesTestAccount()
							wantPath = "/backend-api/codex/images/generations"
						}
						result, err := svc.ForwardImages(context.Background(), c, account, body, parsed, "")
						require.NoError(t, err)
						require.Equal(t, wantPath, upstream.lastReq.URL.Path)
						require.Equal(t, 1, result.ImageCount, "frames must not be billed as separate animations")
						require.Equal(t, []string{"32x16"}, result.ImageOutputSizes)
						forwarded := &OpenAIImagesRequest{Endpoint: openAIImagesGenerationsEndpoint}
						if accountType == AccountTypeAPIKey && multipartRequest {
							require.NoError(t, parseOpenAIImagesMultipartRequest(upstream.lastBody, upstream.lastReq.Header.Get("Content-Type"), forwarded))
						} else {
							require.NoError(t, parseOpenAIImagesJSONRequest(upstream.lastBody, forwarded))
						}
						require.Equal(t, model, forwarded.Model)
						require.Equal(t, "gif", forwarded.OutputFormat)
						require.Equal(t, 3, *forwarded.FrameCount)
						require.Equal(t, 25, *forwarded.FrameDurationMS)
						require.Equal(t, "gif", gjson.GetBytes(rec.Body.Bytes(), "output_format").String())
						require.Equal(t, "gif", gjson.GetBytes(rec.Body.Bytes(), "data.0.output_format").String())
						got := gjson.GetBytes(rec.Body.Bytes(), "data.0.b64_json").String()
						if responseFormat == "url" && accountType == AccountTypeOAuth {
							url := gjson.GetBytes(rec.Body.Bytes(), "data.0.url").String()
							require.True(t, strings.HasPrefix(url, "data:image/gif;base64,"))
							got = strings.TrimPrefix(url, "data:image/gif;base64,")
						}
						require.Equal(t, encoded, got)
					})
				}
			}
		}
	}
}

func TestOpenAIImagesGIFDoesNotFallbackToPNG(t *testing.T) {
	var pngBody bytes.Buffer
	require.NoError(t, png.Encode(&pngBody, image.NewRGBA(image.Rect(0, 0, 32, 16))))
	pngBase64 := base64.StdEncoding.EncodeToString(pngBody.Bytes())
	for _, accountType := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
		body := []byte(`{"model":"gpt-image-2.6-sunburst","prompt":"loop","output_format":"gif"}`)
		c, rec := newOpenAIImagesTestContext(t, body)
		upstream := &httpUpstreamRecorder{resp: &http.Response{
			StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"output_format":"gif","data":[{"b64_json":%q}]}`, pngBase64))),
		}}
		svc := newOpenAIImagesTestService(upstream)
		parsed, err := svc.ParseOpenAIImagesRequest(c, body)
		require.NoError(t, err)
		account := newOpenAIImagesAPIKeyAccount()
		if accountType == AccountTypeOAuth {
			account = directImagesTestAccount()
		}
		result, err := svc.ForwardImages(context.Background(), c, account, body, parsed, "")
		require.ErrorContains(t, err, "non-GIF bytes")
		require.Nil(t, result)
		require.Equal(t, http.StatusBadGateway, rec.Code)
		require.Equal(t, "invalid_gif_output", gjson.GetBytes(rec.Body.Bytes(), "error.code").String())
		require.Len(t, upstream.requests, 1)
		var failover *UpstreamFailoverError
		require.NotErrorAs(t, err, &failover)
	}
	parsed := &OpenAIImagesRequest{Model: "gpt-image-2.6-flare", Prompt: "loop", OutputFormat: "gif", Endpoint: openAIImagesGenerationsEndpoint}
	_, err := buildOpenAIImagesResponsesRequest(parsed, parsed.Model)
	require.ErrorContains(t, err, "Responses fallback is not supported")
	for _, status := range []int{404, 405} {
		body := []byte(`{"model":"gpt-image-2.6-flare","prompt":"loop","output_format":"gif"}`)
		c, _ := newOpenAIImagesTestContext(t, body)
		upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"endpoint unavailable"}}`))}}
		svc := newOpenAIImagesTestService(upstream)
		_, _ = svc.ForwardImages(context.Background(), c, directImagesTestAccount(), body, parsed, "")
		require.Len(t, upstream.requests, 1, "GIF must not replay through the streaming Responses tool")
	}
	account := directImagesTestAccount()
	account.Credentials["model_mapping"] = map[string]any{"gpt-image-2.6-flare": "gpt-image-2"}
	require.ErrorContains(t, parsed.ValidateAccountModels(account, ""), "requires gpt-image-2.6")
}

func TestOpenAIImagesGIFAccessDeniedIsNotRetried(t *testing.T) {
	for _, accountType := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
		t.Run(accountType, func(t *testing.T) {
			body := []byte(`{"model":"gpt-image-2.6-flare","prompt":"loop","output_format":"gif","frame_count":3}`)
			c, rec := newOpenAIImagesTestContext(t, body)
			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: 400, Header: http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{"detail":"GIF output is not enabled for this request."}`)),
			}}
			svc := newOpenAIImagesTestService(upstream)
			parsed, err := svc.ParseOpenAIImagesRequest(c, body)
			require.NoError(t, err)
			account := newOpenAIImagesAPIKeyAccount()
			if accountType == AccountTypeOAuth {
				account = directImagesTestAccount()
			}
			_, err = svc.ForwardImages(context.Background(), c, account, body, parsed, "")
			require.Error(t, err)
			var failover *UpstreamFailoverError
			require.NotErrorAs(t, err, &failover)
			require.Len(t, upstream.requests, 1)
			require.Equal(t, 400, rec.Code)
			require.Contains(t, rec.Body.String(), "GIF output is not enabled for this request.")
		})
	}
}
