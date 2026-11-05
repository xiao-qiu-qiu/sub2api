package service

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"strings"
)

const maxOpenAIImageDimensionProbeBytes int64 = 1 << 20

func detectOpenAIImageResultSize(encoded string) string {
	size, _ := detectOpenAIImageResultMetadata(encoded)
	return size
}

func detectOpenAIImageResultFormat(encoded string) string {
	_, format := detectOpenAIImageResultMetadata(encoded)
	return format
}

func detectOpenAIImageResultMetadata(encoded string) (size, format string) {
	payload := strings.TrimSpace(encoded)
	if strings.HasPrefix(strings.ToLower(payload), "data:") {
		comma := strings.IndexByte(payload, ',')
		if comma < 0 || comma+1 >= len(payload) {
			return "", ""
		}
		payload = strings.TrimSpace(payload[comma+1:])
	}
	if payload == "" {
		return "", ""
	}

	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding} {
		decoded := base64.NewDecoder(encoding, strings.NewReader(payload))
		buffered := bufio.NewReader(io.LimitReader(decoded, maxOpenAIImageDimensionProbeBytes))
		prefix, _ := buffered.Peek(30)
		if width, height, ok := detectOpenAIWebPDimensions(prefix); ok {
			return fmt.Sprintf("%dx%d", width, height), "webp"
		}
		// GIF DecodeConfig reports the logical screen size, even when animation
		// frames cover only part of that canvas.
		cfg, decodedFormat, err := image.DecodeConfig(buffered)
		if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
			continue
		}
		switch decodedFormat {
		case "png", "jpeg", "webp", "gif":
			format = decodedFormat
		}
		return fmt.Sprintf("%dx%d", cfg.Width, cfg.Height), format
	}
	return "", ""
}

func detectOpenAIWebPDimensions(header []byte) (int, int, bool) {
	if len(header) < 16 || string(header[:4]) != "RIFF" || string(header[8:12]) != "WEBP" {
		return 0, 0, false
	}

	switch string(header[12:16]) {
	case "VP8X":
		if len(header) < 30 {
			return 0, 0, false
		}
		width := 1 + int(header[24]) + int(header[25])<<8 + int(header[26])<<16
		height := 1 + int(header[27]) + int(header[28])<<8 + int(header[29])<<16
		return width, height, width > 0 && height > 0
	case "VP8 ":
		if len(header) < 30 || string(header[23:26]) != "\x9d\x01\x2a" {
			return 0, 0, false
		}
		width := int(binary.LittleEndian.Uint16(header[26:28]) & 0x3fff)
		height := int(binary.LittleEndian.Uint16(header[28:30]) & 0x3fff)
		return width, height, width > 0 && height > 0
	case "VP8L":
		if len(header) < 25 || header[20] != 0x2f {
			return 0, 0, false
		}
		width := 1 + int(header[21]) + int(header[22]&0x3f)<<8
		height := 1 + int(header[22]>>6) + int(header[23])<<2 + int(header[24]&0x0f)<<10
		return width, height, width > 0 && height > 0
	default:
		return 0, 0, false
	}
}

func reconcileOpenAIResponsesImageResultSizes(results []openAIResponsesImageResult, firstMeta *openAIResponsesImageResult) {
	var firstActualFormat string
	for i := range results {
		// ChatGPT OAuth can normalize requested controls to "auto". The final
		// image bytes are authoritative for response metadata and tier billing.
		actualSize, actualFormat := detectOpenAIImageResultMetadata(results[i].Result)
		if actualSize != "" {
			results[i].Size = actualSize
		}
		if actualFormat != "" {
			results[i].OutputFormat = actualFormat
			if i == 0 {
				firstActualFormat = actualFormat
			}
		}
	}
	if firstMeta == nil || len(results) == 0 {
		return
	}
	if size := strings.TrimSpace(results[0].Size); size != "" {
		firstMeta.Size = size
	}
	if firstActualFormat != "" {
		firstMeta.OutputFormat = firstActualFormat
	}
}
