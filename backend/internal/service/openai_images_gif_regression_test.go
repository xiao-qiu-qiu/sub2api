package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIImagesGIFRemoteValidation(t *testing.T) {
	gifBytes := gifResponseTestBytes(t)
	for _, accountType := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
		for _, backfill := range []bool{false, true} {
			for _, responseFormat := range []string{"b64_json", "url"} {
				t.Run(fmt.Sprintf("%s/backfill=%t/%s", accountType, backfill, responseFormat), func(t *testing.T) {
					body := []byte(fmt.Sprintf(`{"model":"gpt-image-2.6-flare","prompt":"loop","output_format":"gif","response_format":%q}`, responseFormat))
					c, rec := newOpenAIImagesTestContext(t, body)
					postCalls, downloads := 0, 0
					upstream := &codexModelsHTTPUpstreamStub{do: func(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
						if req.Method == http.MethodPost {
							postCalls++
							return b64BackfillImageResponse(200, "application/json", []byte(`{"output_format":"png","data":[{"url":"https://cdn.example.com/result"},{"url":"https://cdn.example.com/result"}],"usage":{"input_tokens":10,"output_tokens":50}}`)), nil
						}
						downloads++
						require.Equal(t, http.MethodGet, req.Method)
						require.True(t, HTTPUpstreamPublicHostsOnly(req.Context()))
						require.Equal(t, "https://cdn.example.com/result", req.URL.String())
						return b64BackfillImageResponse(200, "image/png", gifBytes), nil
					}}
					svc := newOpenAIImagesTestService(upstream)
					parsed, err := svc.ParseOpenAIImagesRequest(c, body)
					require.NoError(t, err)
					account := newOpenAIImagesAPIKeyAccount()
					if accountType == AccountTypeOAuth {
						account = directImagesTestAccount()
					}
					account.Extra = map[string]any{AccountExtraImagesURLToB64JSON: backfill}
					result, err := svc.ForwardImages(context.Background(), c, account, body, parsed, "")
					require.NoError(t, err)
					require.Equal(t, 1, postCalls)
					require.Equal(t, 1, downloads, "deduplicate repeated URLs and skip a second backfill pass")
					require.Equal(t, 2, result.ImageCount)
					require.Equal(t, 50, result.Usage.ImageOutputTokens)
					require.Equal(t, []string{"32x16", "32x16"}, result.ImageOutputSizes)
					require.Equal(t, "gif", gjson.GetBytes(rec.Body.Bytes(), "output_format").String())
					require.True(t, strings.HasPrefix(gjson.GetBytes(rec.Body.Bytes(), "data.0.url").String(), "data:image/gif;base64,"))
					storageHTTPCalls := 0
					client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
						storageHTTPCalls++
						return nil, fmt.Errorf("validated bytes must not be downloaded again")
					})}
					storage := &fakeImageStorage{}
					out, err := NewImageResultUploader(storage, "images/", 0, client).Rewrite(context.Background(), "verified", rec.Body.Bytes())
					require.NoError(t, err)
					require.Zero(t, storageHTTPCalls)
					require.Len(t, storage.saved, 2)
					for _, saved := range storage.saved {
						require.Equal(t, "image/gif", saved.contentType)
						require.Equal(t, gifBytes, saved.data)
						require.True(t, strings.HasSuffix(saved.key, ".gif"))
					}
					require.Equal(t, "gif", gjson.GetBytes(out, "data.0.output_format").String())
				})
			}
		}
	}
}

func TestOpenAIImagesGIFRemoteRejectsWrongBytesAndPrivateHosts(t *testing.T) {
	var pngBody bytes.Buffer
	require.NoError(t, png.Encode(&pngBody, image.NewRGBA(image.Rect(0, 0, 32, 16))))
	for _, accountType := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
		for _, rawURL := range []string{"https://cdn.example.com/fake.gif", "http://127.0.0.1/private.gif", "http://169.254.169.254/image.gif"} {
			t.Run(accountType+"/"+rawURL, func(t *testing.T) {
				body := []byte(`{"model":"gpt-image-2.6-sunburst","prompt":"loop","output_format":"gif","response_format":"url"}`)
				c, rec := newOpenAIImagesTestContext(t, body)
				posts, gets := 0, 0
				upstream := &codexModelsHTTPUpstreamStub{do: func(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
					if req.Method == http.MethodPost {
						posts++
						payload := []byte(fmt.Sprintf(`{"output_format":"gif","data":[{"url":%q}]}`, rawURL))
						return b64BackfillImageResponse(200, "application/json", payload), nil
					}
					gets++
					require.True(t, HTTPUpstreamPublicHostsOnly(req.Context()))
					return b64BackfillImageResponse(200, "image/gif", pngBody.Bytes()), nil
				}}
				svc := newOpenAIImagesTestService(upstream)
				parsed, err := svc.ParseOpenAIImagesRequest(c, body)
				require.NoError(t, err)
				account := newOpenAIImagesAPIKeyAccount()
				if accountType == AccountTypeOAuth {
					account = directImagesTestAccount()
				}
				result, err := svc.ForwardImages(context.Background(), c, account, body, parsed, "")
				require.Error(t, err)
				require.Nil(t, result)
				require.Equal(t, 502, rec.Code)
				require.Equal(t, "invalid_gif_output", gjson.GetBytes(rec.Body.Bytes(), "error.code").String())
				require.Equal(t, 1, posts)
				wantGets := 0
				if strings.HasPrefix(rawURL, "https://cdn.") {
					wantGets = 1
				}
				require.Equal(t, wantGets, gets)
				var failover *UpstreamFailoverError
				require.NotErrorAs(t, err, &failover)
			})
		}
	}
}

func TestOpenAIImagesGIFEmptyResultIsTerminal(t *testing.T) {
	for _, payload := range []string{`{"data":[]}`, `{"data":[{}]}`, `{"data":[{"b64_json":""}]}`} {
		body := []byte(`{"model":"gpt-image-2.6-flare","prompt":"loop","output_format":"gif"}`)
		c, rec := newOpenAIImagesTestContext(t, body)
		upstream := &httpUpstreamRecorder{resp: b64BackfillImageResponse(200, "application/json", []byte(payload))}
		svc := newOpenAIImagesTestService(upstream)
		parsed, err := svc.ParseOpenAIImagesRequest(c, body)
		require.NoError(t, err)
		_, err = svc.ForwardImages(context.Background(), c, directImagesTestAccount(), body, parsed, "")
		require.Error(t, err)
		require.Equal(t, 502, rec.Code)
		require.Equal(t, "invalid_gif_output", gjson.GetBytes(rec.Body.Bytes(), "error.code").String())
		require.Len(t, upstream.requests, 1)
		var failover *UpstreamFailoverError
		require.NotErrorAs(t, err, &failover)
	}
}

func TestOpenAIImagesGIFFailurePreservesUsageAndEstimatedCost(t *testing.T) {
	for _, accountType := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
		body := []byte(`{"model":"gpt-image-2.6-flare","prompt":"PRIVATE_PROMPT","output_format":"gif"}`)
		c, rec := newOpenAIImagesTestContext(t, body)
		payload := []byte(`{"data":[{"b64_json":"aGVsbG8="}],"usage":{"input_tokens":10,"output_tokens":20}}`)
		upstream := &httpUpstreamRecorder{resp: b64BackfillImageResponse(200, "application/json", payload)}
		upstream.resp.Header.Set("x-request-id", "req_gif_failed")
		svc := newOpenAIImagesTestService(upstream)
		svc.billingService = NewBillingService(&config.Config{}, &PricingService{pricingData: map[string]*LiteLLMModelPricing{}})
		parsed, err := svc.ParseOpenAIImagesRequest(c, body)
		require.NoError(t, err)
		account := newOpenAIImagesAPIKeyAccount()
		if accountType == AccountTypeOAuth {
			account = directImagesTestAccount()
		}
		result, err := svc.ForwardImages(context.Background(), c, account, body, parsed, "")
		require.Nil(t, result)
		var imageErr *OpenAIImagesUpstreamError
		require.ErrorAs(t, err, &imageErr)
		require.NotNil(t, imageErr.UpstreamUsage)
		require.Equal(t, 20, imageErr.UpstreamUsage.ImageOutputTokens)
		require.Equal(t, "req_gif_failed", imageErr.UpstreamRequestID)
		require.Equal(t, 502, rec.Code)
		require.False(t, gjson.GetBytes(rec.Body.Bytes(), "error.usage").Exists())
		value, ok := c.Get(OpsUpstreamErrorsKey)
		require.True(t, ok)
		events := value.([]*OpsUpstreamErrorEvent)
		require.Len(t, events, 1, "one failed upstream attempt must produce one cost observation")
		detail := gjson.Parse(events[0].Detail)
		require.EqualValues(t, 20, detail.Get("usage.images.output_tokens").Int())
		require.InDelta(t, 10*5e-6+20*30e-6, detail.Get("estimated_cost_usd").Float(), 1e-12)
		require.False(t, detail.Get("customer_billed").Bool())
		require.NotContains(t, events[0].Detail, "PRIVATE_PROMPT")
		require.NotContains(t, events[0].Detail, "aGVsbG8=")
		entry := &OpsInsertErrorLogInput{UpstreamErrors: events}
		require.NoError(t, SanitizeOpsUpstreamErrorsForQueue(entry))
		require.NotNil(t, entry.UpstreamErrorsJSON)
		persisted, err := ParseOpsUpstreamErrors(*entry.UpstreamErrorsJSON)
		require.NoError(t, err)
		require.Len(t, persisted, 1)
		storedDetail := gjson.Parse(persisted[0].Detail)
		require.EqualValues(t, 20, storedDetail.Get("usage.images.output_tokens").Int())
		require.InDelta(t, detail.Get("estimated_cost_usd").Float(), storedDetail.Get("estimated_cost_usd").Float(), 1e-12)
	}
}

func TestGPTImage26APIKeyCompletesImageTokenUsage(t *testing.T) {
	for _, model := range []string{"gpt-image-2.6-flare", "gpt-image-2.6-sunburst"} {
		for _, format := range []string{"png", "jpeg", "webp", "gif"} {
			body := []byte(fmt.Sprintf(`{"model":%q,"prompt":"draw","output_format":%q}`, model, format))
			c, _ := newOpenAIImagesTestContext(t, body)
			payload := []byte(fmt.Sprintf(`{"data":[{"b64_json":%q}],"usage":{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"image_tokens":80,"cached_tokens_details":{"image_tokens":40,"text_tokens":10}}}}`, base64.StdEncoding.EncodeToString(gifResponseTestBytes(t))))
			upstream := &httpUpstreamRecorder{resp: b64BackfillImageResponse(200, "application/json", payload)}
			svc := newOpenAIImagesTestService(upstream)
			parsed, err := svc.ParseOpenAIImagesRequest(c, body)
			require.NoError(t, err)
			result, err := svc.ForwardImages(context.Background(), c, newOpenAIImagesAPIKeyAccount(), body, parsed, "")
			require.NoError(t, err)
			require.Equal(t, 20, result.Usage.ImageOutputTokens)
			require.Equal(t, 40, result.Usage.ImageCacheReadTokens)
			require.Equal(t, 50, result.Usage.CacheReadInputTokens)
			pricing, err := NewBillingService(&config.Config{}, &PricingService{pricingData: map[string]*LiteLLMModelPricing{}}).GetModelPricing(model)
			require.NoError(t, err)
			cost := (&BillingService{}).computeTokenBreakdown(pricing, UsageTokens{OutputTokens: result.Usage.OutputTokens, ImageOutputTokens: result.Usage.ImageOutputTokens}, 1, "", false)
			require.InDelta(t, 20*30e-6, cost.ImageOutputCost, 1e-12)
			require.Zero(t, cost.OutputCost)
		}
	}
}

func TestImageResultUploaderRejectsPNGDeclaredGIF(t *testing.T) {
	var body bytes.Buffer
	require.NoError(t, png.Encode(&body, image.NewRGBA(image.Rect(0, 0, 32, 16))))
	for _, itemFormat := range []bool{false, true} {
		item := map[string]string{"b64_json": base64.StdEncoding.EncodeToString(body.Bytes())}
		payload := map[string]any{"data": []map[string]string{item}}
		if itemFormat {
			item["output_format"] = "gif"
		} else {
			payload["output_format"] = "gif"
		}
		encoded, err := json.Marshal(payload)
		require.NoError(t, err)
		storage := &fakeImageStorage{}
		_, err = NewImageResultUploader(storage, "images/", 0, nil).Rewrite(context.Background(), "not_gif", encoded)
		require.ErrorContains(t, err, "expected GIF bytes")
		require.Empty(t, storage.saved)
	}
}

type gifAccessMutationRepo struct {
	AccountRepository
	writes int
}

func (r *gifAccessMutationRepo) SetTempUnschedulable(context.Context, int64, time.Time, string) error {
	r.writes++
	return nil
}
func (r *gifAccessMutationRepo) SetModelRateLimit(context.Context, int64, string, time.Time, ...string) error {
	r.writes++
	return nil
}
func (r *gifAccessMutationRepo) SetError(context.Context, int64, string) error {
	r.writes++
	return nil
}

func TestOpenAIImagesGIFAccessDenialBypassesAccountPolicies(t *testing.T) {
	for _, accountType := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
		for _, exclude400 := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/exclude400=%t", accountType, exclude400), func(t *testing.T) {
				body := []byte(`{"model":"gpt-image-2.6-flare","prompt":"loop","output_format":"gif"}`)
				c, rec := newOpenAIImagesTestContext(t, body)
				payload := []byte(`{"error":{"type":"invalid_request_error","code":"gif_not_enabled","param":"output_format","message":"GIF output is not enabled for this request."}}`)
				upstream := &httpUpstreamRecorder{resp: b64BackfillImageResponse(400, "application/json", payload)}
				svc := newOpenAIImagesTestService(upstream)
				repo := &gifAccessMutationRepo{}
				svc.rateLimitService = NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
				svc.rateLimitService.SetAccountRuntimeBlocker(svc)
				account := newOpenAIImagesAPIKeyAccount()
				if accountType == AccountTypeOAuth {
					account = directImagesTestAccount()
				}
				account.Credentials["temp_unschedulable_enabled"] = true
				account.Credentials["temp_unschedulable_rules"] = []any{map[string]any{
					"error_code": float64(400), "keywords": []any{"GIF output is not enabled"}, "duration_minutes": float64(10),
				}}
				require.Len(t, matchTempUnschedulableRules(account, 400, payload), 1, "fixture must match the custom stop-scheduling rule")
				if exclude400 {
					account.Credentials["custom_error_codes_enabled"] = true
					account.Credentials["custom_error_codes"] = []any{float64(599)}
				}
				parsed, err := svc.ParseOpenAIImagesRequest(c, body)
				require.NoError(t, err)
				_, err = svc.ForwardImages(context.Background(), c, account, body, parsed, "")
				require.Error(t, err)
				var failover *UpstreamFailoverError
				require.NotErrorAs(t, err, &failover)
				require.Equal(t, 400, rec.Code)
				require.JSONEq(t, string(payload), rec.Body.String())
				require.Len(t, upstream.requests, 1)
				require.Zero(t, repo.writes)
				require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
			})
		}
	}
}
