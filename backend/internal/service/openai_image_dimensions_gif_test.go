package service

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/gif"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDetectOpenAIImageResultGIF(t *testing.T) {
	for _, version := range []string{"GIF87a", "GIF89a"} {
		t.Run(version, func(t *testing.T) {
			data := makeOpenAIImageDimensionsTestGIF(t, version)
			animation, err := gif.DecodeAll(bytes.NewReader(data))
			require.NoError(t, err)
			require.Equal(t, 17, animation.Config.Width)
			require.Equal(t, 11, animation.Config.Height)
			if version == "GIF89a" {
				require.Len(t, animation.Image, 2)
				require.NotEqual(t, animation.Config.Width, animation.Image[0].Bounds().Dx())
				require.NotEqual(t, animation.Config.Height, animation.Image[0].Bounds().Dy())
				require.NotZero(t, len(data)%3, "fixture must exercise missing base64 padding")
			} else {
				require.Len(t, animation.Image, 1)
			}

			encoded := base64.StdEncoding.EncodeToString(data)
			unpadded := base64.RawStdEncoding.EncodeToString(data)
			for name, payload := range map[string]string{
				"base64":              encoded,
				"no padding":          unpadded,
				"data URL":            "data:image/gif;base64," + encoded,
				"data URL no padding": " \nDATA:image/gif;BASE64, " + unpadded + "\n ",
				"mismatched MIME":     "data:image/png;base64," + encoded,
			} {
				t.Run(name, func(t *testing.T) {
					require.Equal(t, "17x11", detectOpenAIImageResultSize(payload))
					require.Equal(t, "gif", detectOpenAIImageResultFormat(payload))
				})
			}
		})
	}
}

func TestDetectOpenAIImageResultFormat(t *testing.T) {
	for name, fixture := range map[string]struct {
		encoded string
		format  string
		size    string
	}{
		"png":       {encodeOpenAIImageTestPNG(t, 17, 11), "png", "17x11"},
		"jpeg":      {encodeOpenAIImageTestJPEG(t, 17, 11), "jpeg", "17x11"},
		"webp VP8X": {encodeOpenAIImageTestWebPVP8X(17, 11), "webp", "17x11"},
		"webp VP8":  {encodeOpenAIImageTestWebPVP8(17, 11), "webp", "17x11"},
		"webp VP8L": {encodeOpenAIImageTestWebPVP8L(17, 11), "webp", "17x11"},
	} {
		t.Run(name, func(t *testing.T) {
			for _, payload := range []string{fixture.encoded, strings.TrimRight(fixture.encoded, "="), "data:image/gif;base64," + fixture.encoded} {
				require.Equal(t, fixture.format, detectOpenAIImageResultFormat(payload))
				require.Equal(t, fixture.size, detectOpenAIImageResultSize(payload))
			}
		})
	}

	invalidVersion := makeOpenAIImageDimensionsTestGIF(t, "GIF89a")
	copy(invalidVersion[:6], "GIF90a")
	for _, payload := range []string{
		"", "not-image-data", "data:image/gif;base64", "data:image/gif;base64,",
		base64.StdEncoding.EncodeToString([]byte("GIF89a")),
		base64.StdEncoding.EncodeToString(invalidVersion),
		base64.StdEncoding.EncodeToString([]byte("plain text")),
	} {
		require.Empty(t, detectOpenAIImageResultFormat(payload), payload)
		require.Empty(t, detectOpenAIImageResultSize(payload), payload)
	}
}

func TestReconcileOpenAIResponsesImageResultSizesUsesActualFormats(t *testing.T) {
	gifResult := "data:image/png;base64," + base64.RawStdEncoding.EncodeToString(makeOpenAIImageDimensionsTestGIF(t, "GIF89a"))
	results := []openAIResponsesImageResult{
		{Result: gifResult, OutputFormat: "png", Size: "auto", RevisedPrompt: "kept prompt", Quality: "auto", Model: "kept model", Background: "opaque"},
		{Result: encodeOpenAIImageTestPNG(t, 13, 7), OutputFormat: "gif", Size: "auto"},
		{Result: encodeOpenAIImageTestJPEG(t, 9, 5), OutputFormat: "png", Size: "auto"},
		{Result: encodeOpenAIImageTestWebPVP8X(19, 13), OutputFormat: "jpeg", Size: "auto"},
		{Result: "unknown-image", OutputFormat: "jpeg", Size: "1024x1024"},
	}
	want := append([]openAIResponsesImageResult(nil), results...)
	want[0].OutputFormat, want[0].Size = "gif", "17x11"
	want[1].OutputFormat, want[1].Size = "png", "13x7"
	want[2].OutputFormat, want[2].Size = "jpeg", "9x5"
	want[3].OutputFormat, want[3].Size = "webp", "19x13"
	firstMeta := openAIResponsesImageResult{OutputFormat: "webp", Size: "auto", Quality: "high", Model: "meta model", Background: "transparent", RevisedPrompt: "meta prompt"}
	wantMeta := firstMeta
	wantMeta.OutputFormat, wantMeta.Size = "gif", "17x11"

	reconcileOpenAIResponsesImageResultSizes(results, &firstMeta)

	require.Equal(t, want, results)
	require.Equal(t, wantMeta, firstMeta)
}

func TestReconcileOpenAIResponsesImageResultSizesPreservesFallbacks(t *testing.T) {
	t.Run("nil metadata", func(t *testing.T) {
		results := []openAIResponsesImageResult{{Result: base64.StdEncoding.EncodeToString(makeOpenAIImageDimensionsTestGIF(t, "GIF87a"))}}
		reconcileOpenAIResponsesImageResultSizes(results, nil)
		require.Equal(t, "gif", results[0].OutputFormat)
		require.Equal(t, "17x11", results[0].Size)
	})
	t.Run("no results", func(t *testing.T) {
		meta := openAIResponsesImageResult{Size: "1024x1024", OutputFormat: "png"}
		want := meta
		reconcileOpenAIResponsesImageResultSizes(nil, &meta)
		require.Equal(t, want, meta)
	})
	t.Run("unrecognized bytes", func(t *testing.T) {
		results := []openAIResponsesImageResult{{Result: "unknown", Size: " 1024x1024 ", OutputFormat: " jpeg "}}
		meta := openAIResponsesImageResult{Size: "auto", OutputFormat: "png"}
		reconcileOpenAIResponsesImageResultSizes(results, &meta)
		require.Equal(t, " 1024x1024 ", results[0].Size)
		require.Equal(t, " jpeg ", results[0].OutputFormat)
		require.Equal(t, "1024x1024", meta.Size)
		require.Equal(t, "png", meta.OutputFormat)
	})
	t.Run("empty metadata", func(t *testing.T) {
		results := []openAIResponsesImageResult{{Result: "unknown"}}
		meta := openAIResponsesImageResult{Size: "auto", OutputFormat: "png"}
		want := meta
		reconcileOpenAIResponsesImageResultSizes(results, &meta)
		require.Equal(t, want, meta)
	})
}

func makeOpenAIImageDimensionsTestGIF(t *testing.T, version string) []byte {
	t.Helper()
	palette := color.Palette{color.Black, color.White}
	animation := &gif.GIF{
		Image:     []*image.Paletted{image.NewPaletted(image.Rect(0, 0, 17, 11), palette)},
		Delay:     []int{0},
		LoopCount: -1,
		Config:    image.Config{ColorModel: palette, Width: 17, Height: 11},
	}
	if version == "GIF89a" {
		animation.Image = []*image.Paletted{
			image.NewPaletted(image.Rect(1, 2, 5, 4), palette),
			image.NewPaletted(image.Rect(6, 5, 10, 9), palette),
		}
		animation.Image[1].SetColorIndex(6, 5, 1)
		animation.Delay = []int{3, 7}
		animation.Disposal = []byte{gif.DisposalBackground, gif.DisposalPrevious}
		animation.LoopCount = 0
	}
	var buf bytes.Buffer
	require.NoError(t, gif.EncodeAll(&buf, animation))
	data := buf.Bytes()
	if version == "GIF87a" {
		// The single opaque frame has no GIF89a extensions, so GIF87a is valid.
		copy(data[:6], version)
	} else if len(data)%3 == 0 {
		// A comment keeps the GIF valid while ensuring base64 requires padding.
		data = append(data[:len(data)-1], 0x21, 0xfe, 0x01, 'x', 0x00, 0x3b)
	}
	return data
}
