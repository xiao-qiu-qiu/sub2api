package requestcapture

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const distillationJSON = `{"usage":{"output_tokens":3000,"input_tokens_details":{"cached_tokens":0}}}`

func distillationConfig(m *Manager) Config {
	config := m.Config()
	config.IncludeDistillation, config.DistillationNonStreamOnly = true, true
	config.DistillationMinOutputTokens = 3000
	return config
}

func beginDistillationFixture(t *testing.T, m *Manager, meta Meta) *Session {
	t.Helper()
	var s *Session
	require.Eventually(t, func() bool {
		s = m.Begin(meta)
		return s != nil
	}, time.Second, time.Millisecond)
	return s
}

func writeCaptureResponse(t *testing.T, s *Session, stage string, attempt int, ct, body string, chunkSize int) {
	t.Helper()
	stream := s.NewStream(stage, attempt, 0, ct, nil)
	for offset := 0; offset < len(body); offset += chunkSize {
		_, err := stream.Write([]byte(body[offset:min(offset+chunkSize, len(body))]))
		require.NoError(t, err)
	}
	require.NoError(t, stream.Close())
}

func responsesSSE(body string) string {
	return fmt.Sprintf("data: {\"type\":\"response.completed\",\"response\":%s}\n\n", body)
}

func TestDistillationRetainsOnlyKnownFinalUsage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		path   string
		want   bool
		output int64
	}{
		{name: "responses threshold", body: distillationJSON, want: true, output: 3000},
		{name: "responses large", body: `{"output":[{"text":"` + strings.Repeat("long output ", 8000) + `"}],` + distillationJSON[1:], want: true, output: 3000},
		{name: "short output", body: strings.Replace(distillationJSON, "3000", "2999", 1)},
		{name: "positive cache", body: strings.Replace(distillationJSON, `"cached_tokens":0`, `"cached_tokens":1`, 1)},
		{name: "nested cache overrides flat zero", body: `{"usage":{"output_tokens":3000,"cache_read_input_tokens":0,"input_tokens_details":{"cached_tokens":25}}}`},
		{name: "nested zero overrides flat cache", body: `{"usage":{"output_tokens":3000,"cache_read_input_tokens":25,"input_tokens_details":{"cached_tokens":0}}}`, want: true, output: 3000},
		{name: "nested unknown overrides flat zero", body: `{"usage":{"output_tokens":3000,"cached_tokens":0,"prompt_tokens_details":{"cached_tokens":null}}}`},
		{name: "flat cached tokens", body: `{"usage":{"output_tokens":3000,"cached_tokens":0}}`, want: true, output: 3000},
		{name: "provider alias reports cache", body: `{"usage":{"output_tokens":3000,"cache_read_input_tokens":0,"cached_tokens":25}}`},
		{name: "cache missing", body: `{"usage":{"output_tokens":5000}}`},
		{name: "duplicate usage invalidates first value", body: distillationJSON[:len(distillationJSON)-1] + `,"usage":null}`},
		{name: "duplicate details invalidate first value", body: `{"usage":{"output_tokens":3000,"input_tokens_details":{"cached_tokens":0},"input_tokens_details":null}}`},
		{name: "duplicate cache invalidates first value", body: `{"usage":{"output_tokens":3000,"input_tokens_details":{"cached_tokens":0,"cached_tokens":null}}}`},
		{name: "cache null", body: strings.Replace(distillationJSON, `"cached_tokens":0`, `"cached_tokens":null`, 1)},
		{name: "cache string", body: strings.Replace(distillationJSON, `"cached_tokens":0`, `"cached_tokens":"0"`, 1)},
		{name: "cache negative", body: strings.Replace(distillationJSON, `"cached_tokens":0`, `"cached_tokens":-1`, 1)},
		{name: "cache decimal", body: strings.Replace(distillationJSON, `"cached_tokens":0`, `"cached_tokens":0.25`, 1)},
		{name: "cache overflow", body: strings.Replace(distillationJSON, `"cached_tokens":0`, `"cached_tokens":9223372036854775808`, 1)},
		{name: "output null", body: strings.Replace(distillationJSON, "3000", "null", 1)},
		{name: "fake nested usage", body: `{"output":[` + distillationJSON + `]}`},
		{name: "truncated response", body: distillationJSON[:len(distillationJSON)-1]},
		{name: "chat", body: `{"usage":{"completion_tokens":5000,"prompt_tokens_details":{"cached_tokens":0}}}`, want: true, output: 5000},
		{name: "deepseek", body: `{"usage":{"completion_tokens":3000,"prompt_cache_hit_tokens":0}}`, want: true, output: 3000},
		{name: "anthropic", body: `{"usage":{"output_tokens":3200,"cache_read_input_tokens":0}}`, want: true, output: 3200},
		{name: "gemini", body: `{"usageMetadata":{"candidatesTokenCount":2999,"thoughtsTokenCount":4,"cachedContentTokenCount":0}}`, want: true, output: 3003},
		{name: "gemini missing cache", body: `{"usageMetadata":{"candidatesTokenCount":3500}}`},
		{name: "gemini stream endpoint", path: "/v1beta/models/gemini:streamGenerateContent?alt=sse", body: `{"usageMetadata":{"candidatesTokenCount":3000,"cachedContentTokenCount":0}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, store := testManager(t)
			m.ApplyConfig(distillationConfig(m))
			target := task(t, m, "user", 1, false)
			s := beginDistillationFixture(t, m, Meta{UserID: 1, Path: tc.path, Protocol: "http"})
			s.ClientRequest([]byte(`{"stream":false,"input":"private fixture"}`), "application/json", nil)
			writeCaptureResponse(t, s, "client_response", 0, "application/json", tc.body, 7)
			s.Finish(http.StatusOK)
			drain(t, m)
			rows, err := m.Records(t.Context(), target.ID, "", false, 10, 0)
			require.NoError(t, err)
			if !tc.want {
				require.Empty(t, rows)
				require.Zero(t, m.Stats().UsedBytes)
				require.Empty(t, store.records)
				return
			}
			require.Len(t, rows, 1)
			require.True(t, rows[0].IsSuspectedDistillation)
			require.False(t, rows[0].IsError)
			require.False(t, rows[0].Partial)
			require.Equal(t, &DistillationEvidence{OutputTokens: tc.output}, rows[0].DistillationEvidence)
			require.Equal(t, tc.output, rows[0].Usage["output_tokens"])
			require.Contains(t, rows[0].Usage, "cache_read_tokens")
		})
	}
}

func TestDistillationUsesFinalAttemptAndClientStreamMode(t *testing.T) {
	for _, tc := range []struct {
		name                string
		clientCT, client    string
		upstream            string
		previous            string
		includeStream, fail bool
		want                bool
	}{
		{name: "HTTP JSON from upstream SSE", client: `{"output":[]}`, upstream: responsesSSE(distillationJSON), want: true},
		{name: "client response stream blocked", clientCT: "text/event-stream", client: responsesSSE(distillationJSON), upstream: responsesSSE(distillationJSON)},
		{name: "client response stream enabled", clientCT: "text/event-stream", client: responsesSSE(distillationJSON), upstream: responsesSSE(distillationJSON), includeStream: true, want: true},
		{name: "previous known cache cannot fill final usage", client: distillationJSON, upstream: responsesSSE(`{"usage":{"output_tokens":4000}}`), previous: distillationJSON, fail: true},
		{name: "previous known output cannot fill final usage", client: distillationJSON, upstream: responsesSSE(`{"usage":{"input_tokens_details":{"cached_tokens":0}}}`), previous: distillationJSON, fail: true},
		{name: "retried completion matches", client: distillationJSON, upstream: responsesSSE(distillationJSON), previous: `{"usage":{"output_tokens":10,"input_tokens_details":{"cached_tokens":800}}}`, fail: true, want: true},
		{name: "intermediate response usage ignored", client: distillationJSON, upstream: "data: {\"type\":\"response.in_progress\",\"response\":" + distillationJSON + "}\n\n" + responsesSSE(`{}`)},
		{name: "responses cannot use chat terminal", client: `{}`, upstream: "data: {\"type\":\"response.created\"}\n\ndata: {\"choices\":[],\"usage\":{\"completion_tokens\":5000,\"prompt_cache_hit_tokens\":0}}\n\ndata: [DONE]\n\n"},
		{name: "arbitrary usage followed by chat terminal", client: `{}`, upstream: "data: " + distillationJSON + "\n\ndata: [DONE]\n\n"},
		{name: "anthropic cannot use chat terminal", client: `{}`, upstream: "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"cache_read_input_tokens\":0}}}\n\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3000}}\n\ndata: [DONE]\n\n"},
		{name: "anthropic split terminal usage", client: `{}`, upstream: "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"output_tokens\":0,\"cache_read_input_tokens\":0}}}\n\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3000}}\n\ndata: {\"type\":\"message_stop\"}\n\n", want: true},
		{name: "chat final usage chunk", client: `{}`, upstream: "data: {\"choices\":[],\"usage\":{\"completion_tokens\":3000,\"prompt_tokens_details\":{\"cached_tokens\":0}}}\n\ndata: [DONE]\n\n", want: true},
		{name: "gemini final usage chunk", client: `{}`, upstream: "data: {\"response\":{\"candidates\":[{\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"candidatesTokenCount\":3000,\"cachedContentTokenCount\":0}}}\n\n", want: true},
		{name: "gemini with compatibility sentinel", client: `{}`, upstream: "data: {\"candidates\":[{\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"candidatesTokenCount\":3000,\"cachedContentTokenCount\":0}}\n\ndata: [DONE]\n\n", want: true},
		{name: "gemini final cache unknown", client: `{}`, upstream: "data: {\"usageMetadata\":{\"candidatesTokenCount\":10,\"cachedContentTokenCount\":0}}\n\ndata: {\"candidates\":[{\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"candidatesTokenCount\":3000}}\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := testManager(t)
			config := distillationConfig(m)
			config.DistillationNonStreamOnly = !tc.includeStream
			m.ApplyConfig(config)
			target := task(t, m, "user", 1, false)
			s := beginDistillationFixture(t, m, Meta{UserID: 1, Protocol: "http"})
			s.ClientRequest([]byte(`{"stream":false}`), "application/json", nil)
			if tc.previous != "" {
				previous := s.BeginAttempt(1)
				s.AttemptResponse(previous, http.StatusServiceUnavailable, nil, nil)
				writeCaptureResponse(t, s, "upstream_response", previous, "application/json", tc.previous, 32768)
			}
			attempt := s.BeginAttempt(2)
			s.AttemptResponse(attempt, http.StatusOK, nil, nil)
			writeCaptureResponse(t, s, "upstream_response", attempt, "text/event-stream", tc.upstream, 97)
			ct := tc.clientCT
			if ct == "" {
				ct = "application/json"
			}
			writeCaptureResponse(t, s, "client_response", 0, ct, tc.client, 97)
			s.Finish(http.StatusOK)
			drain(t, m)
			rows, err := m.Records(t.Context(), target.ID, "", false, 10, 0)
			require.NoError(t, err)
			if tc.want || tc.fail {
				require.Len(t, rows, 1)
				require.Equal(t, tc.want, rows[0].IsSuspectedDistillation)
				require.Equal(t, tc.fail, rows[0].IsError)
				if tc.want {
					require.Equal(t, &DistillationEvidence{OutputTokens: 3000, ClientStream: tc.includeStream}, rows[0].DistillationEvidence)
				}
			} else {
				require.Empty(t, rows)
			}
		})
	}
}

func TestDistillationSnapshotAndRetainedRecordAccess(t *testing.T) {
	m, _ := testManager(t)
	target := task(t, m, "user", 1, false)
	before := beginDistillationFixture(t, m, Meta{UserID: 1, RequestID: "before"})
	config := distillationConfig(m)
	config.DistillationMaxCacheReadTokens = 7
	m.ApplyConfig(config)
	eligible := beginDistillationFixture(t, m, Meta{UserID: 1, RequestID: "eligible"})
	config.IncludeDistillation = false
	config.DistillationMinOutputTokens++
	config.DistillationMaxCacheReadTokens--
	m.ApplyConfig(config)
	after := beginDistillationFixture(t, m, Meta{UserID: 1, RequestID: "after"})
	for _, s := range []*Session{before, eligible, after} {
		s.ClientRequest([]byte(`{"input":"redacted fixture","api_key":"PRIVATE"}`), "application/json", nil)
		writeCaptureResponse(t, s, "client_response", 0, "application/json", strings.Replace(distillationJSON, `"cached_tokens":0`, `"cached_tokens":7`, 1), 97)
		s.Finish(http.StatusOK)
	}
	failed := beginDistillationFixture(t, m, Meta{UserID: 1, RequestID: "error"})
	failed.Finish(http.StatusBadGateway)
	drain(t, m)
	all, err := m.Records(t.Context(), target.ID, "", false, 10, 0)
	require.NoError(t, err)
	require.Len(t, all, 2)
	failures, err := m.Records(t.Context(), target.ID, "", true, 10, 0)
	require.NoError(t, err)
	require.Len(t, failures, 1)
	require.Equal(t, "error", failures[0].RequestID)
	rows, err := m.Records(t.Context(), target.ID, "eligible", false, 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	row, err := m.Record(t.Context(), target.ID, rows[0].ID)
	require.NoError(t, err)
	require.Equal(t, &DistillationEvidence{OutputTokens: 3000, CacheReadTokens: 7}, row.DistillationEvidence)
	text, _, _, err := m.Preview(t.Context(), target.ID, row.ID, row.Parts[0].Name, 0)
	require.NoError(t, err)
	require.Contains(t, text, "redacted fixture")
	require.NotContains(t, text, "PRIVATE")
	saved, err := m.Task(t.Context(), target.ID)
	require.NoError(t, err)
	require.EqualValues(t, 2, saved.Requests)
	require.Equal(t, row.Bytes, saved.Bytes)
	require.NoError(t, m.Stop(t.Context(), target.ID))
	var archive bytes.Buffer
	require.NoError(t, m.Export(t.Context(), &archive, target.ID, ""))
	compressed, err := gzip.NewReader(&archive)
	require.NoError(t, err)
	defer func() { require.NoError(t, compressed.Close()) }()
	reader := tar.NewReader(compressed)
	metadata := map[string]string{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if strings.HasSuffix(header.Name, "/record.json") {
			data, err := io.ReadAll(reader)
			require.NoError(t, err)
			metadata[header.Name] = string(data)
		}
	}
	require.Len(t, metadata, 2)
	require.Contains(t, metadata[row.ID+"/record.json"], `"is_suspected_distillation": true`)
}

func TestDistillationDoesNotRetainHTTPResponseFinishedAfterCutoff(t *testing.T) {
	for _, cutoff := range []string{"stop", "expire", "disable"} {
		t.Run(cutoff, func(t *testing.T) {
			m, _ := testManager(t)
			config := distillationConfig(m)
			m.ApplyConfig(config)
			target := task(t, m, "user", 1, false)
			s := beginDistillationFixture(t, m, Meta{UserID: 1})
			s.ClientRequest([]byte(`{}`), "application/json", nil)
			writeCaptureResponse(t, s, "client_response", 0, "application/json", distillationJSON, 1024)
			require.Eventually(t, func() bool { return m.Stats().UsedBytes > 0 }, time.Second, time.Millisecond)
			switch cutoff {
			case "stop":
				require.NoError(t, m.Stop(t.Context(), target.ID))
			case "expire":
				m.mu.Lock()
				m.tasks[target.ID].task.ExpiresAt = time.Now().Add(-time.Millisecond)
				m.mu.Unlock()
			case "disable":
				config.Enabled = false
				m.ApplyConfig(config)
			}
			s.Finish(http.StatusOK)
			drain(t, m)
			rows, err := m.Records(t.Context(), target.ID, "", false, 10, 0)
			require.NoError(t, err)
			require.Empty(t, rows)
			require.Zero(t, m.Stats().UsedBytes)
		})
	}
}

func TestDistillationWebSocketStreamConfigurationAndTurnIsolation(t *testing.T) {
	for _, includeStreams := range []bool{false, true} {
		t.Run(fmt.Sprint(includeStreams), func(t *testing.T) {
			m, _ := testManager(t)
			config := distillationConfig(m)
			config.DistillationNonStreamOnly = !includeStreams
			m.ApplyConfig(config)
			target := task(t, m, "user", 1, false)
			s := beginDistillationFixture(t, m, Meta{UserID: 1, Protocol: "websocket"})
			for _, usage := range []string{distillationJSON, `{"usage":{"output_tokens":5000}}`} {
				s.ClientFrame([]byte(`{"type":"response.create","stream":false}`))
				attempt := s.WSRequest(8, []byte(`{"type":"response.create"}`))
				s.AttemptResponse(attempt, http.StatusSwitchingProtocols, nil, nil)
				frame := []byte(fmt.Sprintf(`{"type":"response.completed","response":%s}`, usage))
				s.Frame("upstream_response", attempt, frame)
				s.Frame("client_response", 0, frame)
			}
			require.Eventually(t, func() bool { return s.pending.Load() == 0 }, 5*time.Second, time.Millisecond)
			rows, err := m.Records(context.Background(), target.ID, "", false, 10, 0)
			require.NoError(t, err)
			if includeStreams {
				require.Len(t, rows, 1)
				require.Equal(t, 1, rows[0].Turn)
				require.False(t, rows[0].IsError)
				require.True(t, rows[0].DistillationEvidence.ClientStream)
			} else {
				require.Empty(t, rows)
			}
			s.Finish(http.StatusSwitchingProtocols)
			drain(t, m)
		})
	}
}
