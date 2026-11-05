package service

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// WriteOpenAIImagesUpstreamErrorResponse also handles errors produced after a
// successful upstream HTTP status. Already-written errors are preserved.
func WriteOpenAIImagesUpstreamErrorResponse(c *gin.Context, err *OpenAIImagesUpstreamError) bool {
	return writeOpenAIImagesUpstreamErrorResponse(c, err)
}

func supportsOpenAIImagesGIF(model string) bool {
	switch strings.TrimSpace(model) {
	case "gpt-image-2.6-flare", "gpt-image-2.6-sunburst":
		return true
	default:
		return false
	}
}

// GIF limits follow the image generation guide. The Python reference still
// lists an older 16-frame preview limit; the guide allows 2–64 frames.
func validateOpenAIImagesGIFRequest(req *OpenAIImagesRequest, model string) error {
	if req == nil {
		return nil
	}
	if req.OutputFormat != "gif" {
		if req.FrameCount != nil || req.FrameDurationMS != nil {
			return fmt.Errorf("frame_count and frame_duration_ms require output_format=gif")
		}
		return nil
	}
	if !supportsOpenAIImagesGIF(model) {
		return fmt.Errorf("GIF output requires gpt-image-2.6-flare or gpt-image-2.6-sunburst")
	}
	if req.Endpoint != openAIImagesGenerationsEndpoint {
		return fmt.Errorf("GIF output is only supported on images/generations; GIF edits are not supported")
	}
	if req.Stream {
		return fmt.Errorf("GIF output does not support stream")
	}
	if req.PartialImages != nil {
		return fmt.Errorf("GIF output does not support partial_images")
	}
	if req.HasMask || req.MaskUpload != nil || req.MaskImageURL != "" {
		return fmt.Errorf("GIF output does not support mask")
	}
	if req.OutputCompression != nil {
		return fmt.Errorf("GIF output does not support output_compression")
	}
	if req.HasPromptRewriter {
		return fmt.Errorf("GIF output does not support prompt_rewriter")
	}
	if req.FrameCount != nil && (*req.FrameCount < 2 || *req.FrameCount > 64) {
		return fmt.Errorf("frame_count must be between 2 and 64")
	}
	if req.FrameDurationMS != nil && (*req.FrameDurationMS < 20 || *req.FrameDurationMS > 2000) {
		return fmt.Errorf("frame_duration_ms must be between 20 and 2000")
	}
	// The upstream chooses defaults, rounds delays to 10 ms, and enforces its
	// model-specific pixel budget. Preserve omitted fields and supplied delays.
	if req.Size != "" && req.Size != "auto" {
		width, height, ok := strings.Cut(req.Size, "x")
		w, ew := strconv.Atoi(width)
		h, eh := strconv.Atoi(height)
		if !ok || ew != nil || eh != nil || w <= 0 || h <= 0 || w%16 != 0 || h%16 != 0 {
			return fmt.Errorf("GIF size must be auto or positive WIDTHxHEIGHT multiples of 16")
		}
	}
	return nil
}

// Normalize GIF metadata without re-encoding any frames. A raster response to
// an explicit GIF request is an upstream error, never a successful PNG fallback.
func normalizeOpenAIImagesGIFResponse(body []byte, req *OpenAIImagesRequest) ([]byte, error) {
	if req == nil || req.OutputFormat != "gif" {
		return body, nil
	}
	fail := func(message string) ([]byte, error) {
		return nil, &OpenAIImagesUpstreamError{StatusCode: 502, ErrorType: "upstream_error", Code: "invalid_gif_output", Message: message}
	}
	if !gjson.ValidBytes(body) || !gjson.GetBytes(body, "data").IsArray() || len(gjson.GetBytes(body, "data").Array()) == 0 {
		return fail("Images API returned no GIF output")
	}
	for i, item := range gjson.GetBytes(body, "data").Array() {
		encoded := item.Get("b64_json").String()
		if encoded == "" && strings.HasPrefix(strings.ToLower(item.Get("url").String()), "data:") {
			encoded = item.Get("url").String()
		}
		if encoded != "" {
			encoded = normalizeOpenAIImageBase64(encoded)
			if detectOpenAIImageResultFormat(encoded) != "gif" {
				return fail("Images API returned non-GIF bytes for output_format=gif")
			}
			if size := detectOpenAIImageResultSize(encoded); size != "" {
				body, _ = sjson.SetBytes(body, fmt.Sprintf("data.%d.size", i), size)
				if i == 0 {
					body, _ = sjson.SetBytes(body, "size", size)
				}
			}
			body, _ = sjson.SetBytes(body, fmt.Sprintf("data.%d.b64_json", i), encoded)
			if strings.TrimSpace(item.Get("url").String()) != "" {
				body, _ = sjson.SetBytes(body, fmt.Sprintf("data.%d.url", i), "data:image/gif;base64,"+encoded)
			}
		} else {
			return fail("Images API returned no verified GIF bytes")
		}
		body, _ = sjson.SetBytes(body, fmt.Sprintf("data.%d.output_format", i), "gif")
	}
	body, _ = sjson.SetBytes(body, "output_format", "gif")
	return body, nil
}

// Remote URLs use the existing public-host-only downloader, account proxy,
// allowlist, redirect policy and byte limit. Keep verified bytes for storage so
// a mutable URL cannot change format between validation and object upload.
func (s *OpenAIGatewayService) prepareOpenAIImagesGIFResponse(ctx context.Context, c *gin.Context, account *Account, resp *http.Response, body []byte, req *OpenAIImagesRequest) ([]byte, error) {
	if req == nil || req.OutputFormat != "gif" {
		return body, nil
	}
	originalBody := body
	downloaded := make(map[string]string)
	var prepareErr error
	for i, item := range gjson.GetBytes(body, "data").Array() {
		if strings.TrimSpace(item.Get("b64_json").String()) != "" {
			continue
		}
		rawURL := strings.TrimSpace(item.Get("url").String())
		if rawURL == "" {
			continue
		}
		encoded, ok := downloaded[rawURL]
		if !ok {
			var err error
			encoded, err = s.fetchOpenAIImageURLBase64(ctx, account, rawURL)
			if err != nil {
				prepareErr = &OpenAIImagesUpstreamError{StatusCode: http.StatusBadGateway, ErrorType: "upstream_error", Code: "invalid_gif_output", Message: "Unable to verify GIF output: " + sanitizeUpstreamErrorMessage(err.Error())}
				break
			}
			downloaded[rawURL] = encoded
		}
		body, _ = sjson.SetBytes(body, fmt.Sprintf("data.%d.b64_json", i), encoded)
	}
	if prepareErr == nil {
		body, prepareErr = normalizeOpenAIImagesGIFResponse(body, req)
	}
	if prepareErr != nil {
		s.recordOpenAIImagesGIFOutputFailure(c, account, resp, req, originalBody, prepareErr)
		return nil, prepareErr
	}
	return body, nil
}
