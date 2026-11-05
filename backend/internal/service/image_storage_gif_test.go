package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// imageStorageGIFTestFixture uses opaque frames so the single-frame GIF has no
// GIF89a extensions and can legally use a GIF87a signature. GIF87a varies the
// single frame's width; GIF89a uses a comment to set the base64 padding exactly.
func imageStorageGIFTestFixture(t *testing.T, version string, padding int) []byte {
	t.Helper()
	palette := color.Palette{
		color.RGBA{R: 255, A: 255},
		color.RGBA{G: 255, A: 255},
		color.RGBA{B: 255, A: 255},
		color.RGBA{R: 255, G: 255, B: 255, A: 255},
	}
	frames := 1
	if version == "GIF89a" {
		frames = 3
	}
	for width := 2; width <= 128; width++ {
		animation := &gif.GIF{
			LoopCount: -1,
			Config:    image.Config{ColorModel: palette, Width: width, Height: 3},
		}
		for frame := 0; frame < frames; frame++ {
			img := image.NewPaletted(image.Rect(0, 0, width, 3), palette)
			for y := 0; y < 3; y++ {
				for x := 0; x < width; x++ {
					img.SetColorIndex(x, y, uint8((x+y+frame)%len(palette)))
				}
			}
			animation.Image = append(animation.Image, img)
			animation.Delay = append(animation.Delay, 0)
		}
		if frames > 1 {
			animation.Delay = []int{3, 7, 11}
			animation.Disposal = []byte{gif.DisposalNone, gif.DisposalBackground, gif.DisposalPrevious}
			animation.LoopCount = 4
		}
		var encoded bytes.Buffer
		require.NoError(t, gif.EncodeAll(&encoded, animation))
		data := encoded.Bytes()
		if version == "GIF89a" {
			// A comment adds 4+N bytes before the trailer. Choosing N=2+delta
			// adjusts len(data)%3 exactly, independently of LZW compression.
			delta := (3 - padding - len(data)%3 + 3) % 3
			commentLength := 2 + delta
			comment := []byte{0x21, 0xfe, byte(commentLength)}
			comment = append(comment, bytes.Repeat([]byte{'x'}, commentLength)...)
			comment = append(comment, 0x00, 0x3b)
			require.Equal(t, byte(0x3b), data[len(data)-1])
			data = append(data[:len(data)-1], comment...)
		}
		if (3-len(data)%3)%3 != padding {
			continue
		}
		if version == "GIF87a" {
			copy(data[:6], "GIF87a")
		}
		require.Equal(t, version, string(data[:6]))
		decoded, err := gif.DecodeAll(bytes.NewReader(data))
		require.NoError(t, err, "fixture must be a real GIF")
		require.Len(t, decoded.Image, frames)
		require.Equal(t, animation.Delay, decoded.Delay)
		require.Equal(t, animation.LoopCount, decoded.LoopCount)
		if frames > 1 {
			require.Equal(t, animation.Disposal, decoded.Disposal)
		}
		return data
	}
	t.Fatal("could not generate GIF fixture with requested base64 padding")
	return nil
}

func TestImageResultUploaderGIFPreservesOriginalBytes(t *testing.T) {
	for _, version := range []string{"GIF87a", "GIF89a"} {
		for _, padding := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s/padding_%d", version, padding), func(t *testing.T) {
				data := imageStorageGIFTestFixture(t, version, padding)
				padded := base64.StdEncoding.EncodeToString(data)
				require.True(t, strings.HasSuffix(padded, strings.Repeat("=", padding)))
				unpadded := strings.TrimRight(padded, "=")
				require.Equal(t, padding, len(padded)-len(unpadded))
				wantGIF, err := gif.DecodeAll(bytes.NewReader(data))
				require.NoError(t, err)
				tests := []struct {
					name        string
					field       string
					value       string
					remote      bool
					contentType string
				}{
					{name: "b64_json", field: "b64_json", value: padded},
					{name: "b64_json_unpadded", field: "b64_json", value: unpadded},
					{name: "b64_json_unpadded_whitespace", field: "b64_json", value: " \n" + unpadded + "\r\n "},
					{name: "data_url", field: "url", value: "data:image/gif;base64," + padded},
					{name: "data_url_unpadded", field: "url", value: "data:image/gif;base64," + unpadded},
					{name: "data_url_unpadded_crlf", field: "url", value: "data:image/gif;base64," + unpadded[:8] + "\r\n" + unpadded[8:]},
					{name: "data_url_wrong_mime", field: "url", value: "DATA:image/png;name=photo.png;BaSe64," + padded},
					{name: "url_gif", remote: true, contentType: "Image/GIF; charset=binary"},
					{name: "url_octet_stream", remote: true, contentType: "application/octet-stream"},
					{name: "url_wrong_mime", remote: true, contentType: "image/png"},
					{name: "url_missing_mime", remote: true},
				}
				for _, tt := range tests {
					t.Run(tt.name, func(t *testing.T) {
						const sourceURL = "https://images.example.test/without-extension"
						httpCalls := 0
						client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
							httpCalls++
							if !tt.remote {
								return nil, errors.New("inline GIF must not invoke HTTP")
							}
							require.Equal(t, http.MethodGet, req.Method)
							require.Equal(t, sourceURL, req.URL.String())
							return &http.Response{
								StatusCode: http.StatusOK,
								Header:     http.Header{"Content-Type": []string{tt.contentType}},
								Body:       io.NopCloser(bytes.NewReader(data)),
								Request:    req,
							}, nil
						})}
						item := map[string]string{tt.field: tt.value, "revised_prompt": "kept"}
						if tt.remote {
							item = map[string]string{"url": sourceURL, "revised_prompt": "kept"}
						}
						result, err := json.Marshal(map[string]any{"created": 1, "data": []map[string]string{item}})
						require.NoError(t, err)
						storage := &fakeImageStorage{}
						uploader := NewImageResultUploader(storage, "images/", 0, client)
						out, err := uploader.Rewrite(context.Background(), "gif_task", result)
						require.NoError(t, err)
						wantCalls := 0
						if tt.remote {
							wantCalls = 1
						}
						require.Equal(t, wantCalls, httpCalls)
						require.Len(t, storage.saved, 1)
						saved := storage.saved[0]
						require.Equal(t, "image/gif", saved.contentType)
						require.Equal(t, "images/gif_task-0.gif", saved.key)
						require.Equal(t, data, saved.data, "storage must preserve the complete GIF without re-encoding")
						gotGIF, err := gif.DecodeAll(bytes.NewReader(saved.data))
						require.NoError(t, err)
						require.Equal(t, wantGIF, gotGIF, "all frames and animation metadata must survive")
						require.JSONEq(t, `{"created":1,"data":[{"url":"https://cdn.test/images/gif_task-0.gif","revised_prompt":"kept"}]}`, string(out))
					})
				}
			})
		}
	}
}

func TestImageResultUploaderGIFSizeLimits(t *testing.T) {
	data := imageStorageGIFTestFixture(t, "GIF89a", 2)
	for _, source := range []string{"data_url", "data_url_unpadded", "url"} {
		for _, oversized := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/oversized_%t", source, oversized), func(t *testing.T) {
				payload := base64.StdEncoding.EncodeToString(data)
				if source == "data_url_unpadded" {
					payload = strings.TrimRight(payload, "=")
				}
				value := "data:image/gif;base64," + payload
				if source == "url" {
					value = "https://images.example.test/animation.gif"
				}
				httpCalls := 0
				client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					httpCalls++
					if source != "url" {
						return nil, errors.New("data URL must not invoke HTTP")
					}
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"image/gif"}},
						Body:       io.NopCloser(bytes.NewReader(data)),
						Request:    req,
					}, nil
				})}
				limit := int64(len(data))
				if oversized {
					limit--
				}
				storage := &fakeImageStorage{}
				uploader := NewImageResultUploader(storage, "images/", limit, client)
				result, err := json.Marshal(map[string]any{"data": []map[string]string{{"url": value}}})
				require.NoError(t, err)
				_, err = uploader.Rewrite(context.Background(), "gif_limit", result)
				if oversized {
					require.ErrorContains(t, err, fmt.Sprintf("exceeds %d bytes", limit))
					require.Empty(t, storage.saved)
				} else {
					require.NoError(t, err)
					require.Len(t, storage.saved, 1)
					require.Equal(t, data, storage.saved[0].data)
				}
				wantCalls := 0
				if source == "url" {
					wantCalls = 1
				}
				require.Equal(t, wantCalls, httpCalls)
			})
		}
	}
}

func TestImageResultUploaderGIFRejectsMalformedBase64(t *testing.T) {
	for _, source := range []string{"b64_json", "data_url"} {
		for _, payload := range []string{"A", "AA=", "AA===", "AA*A", "AA-_", "AA==AA", "AA==!", "AA==\n="} {
			t.Run(source+"/"+payload, func(t *testing.T) {
				item := map[string]string{"b64_json": payload}
				if source == "data_url" {
					item = map[string]string{"url": "data:image/gif;base64," + payload}
				}
				result, err := json.Marshal(map[string]any{"data": []map[string]string{item}})
				require.NoError(t, err)
				storage := &fakeImageStorage{}
				httpCalls := 0
				client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					httpCalls++
					return nil, errors.New("invalid inline image must not invoke HTTP")
				})}
				uploader := NewImageResultUploader(storage, "images/", 0, client)
				_, err = uploader.Rewrite(context.Background(), "gif_invalid", result)
				require.Error(t, err)
				require.Empty(t, storage.saved)
				require.Zero(t, httpCalls)
			})
		}
	}
}
