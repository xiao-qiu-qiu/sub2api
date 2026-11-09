package repository

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/accountrequest"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestHTTPUpstreamQualityInterruptBeforeHeaders(t *testing.T) {
	started := make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	upstream := NewHTTPUpstream(nil)
	const accountID = 9961801
	done := make(chan error, 1)
	go func() {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
		_, err := upstream.Do(req, "", accountID, 2)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not receive request")
	}
	require.Positive(t, accountrequest.Default.Cancel(accountID, time.Now()))
	select {
	case err := <-done:
		require.ErrorIs(t, err, accountrequest.ErrQualityDegraded)
	case <-time.After(time.Second):
		t.Fatal("quality interrupt did not stop the header wait")
	}
	require.EqualValues(t, 1, calls.Load())
	require.Zero(t, accountrequest.Default.Cancel(accountID, time.Now()))
}

func TestHTTPUpstreamQualityInterruptAllBodiesAndIsolateAccount(t *testing.T) {
	for _, fingerprint := range []bool{false, true} {
		t.Run(fmt.Sprint(fingerprint), func(t *testing.T) {
			finishOther := make(chan struct{})
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: partial\n\n")
				flusher, ok := w.(http.Flusher)
				require.True(t, ok)
				flusher.Flush()
				if r.URL.Path == "/other" {
					select {
					case <-finishOther:
						_, _ = io.WriteString(w, "data: complete\n\n")
					case <-r.Context().Done():
					}
					return
				}
				<-r.Context().Done()
			}))
			defer server.Close()
			upstream, ok := NewHTTPUpstream(nil).(*httpUpstreamService)
			require.True(t, ok)
			accountID := int64(9961810)
			if fingerprint {
				accountID++
			}
			var profile *tlsfingerprint.Profile
			if fingerprint {
				profile = &tlsfingerprint.Profile{Name: "quality-test"}
			}
			// Trust only this test server while exercising Do/DoWithTLS's real
			// response lifetime and request cancellation, including blocked reads.
			for _, id := range []int64{accountID, accountID + 100} {
				var entry *upstreamClientEntry
				var err error
				if profile == nil {
					entry, err = upstream.getClientEntry("", id, 3, service.HTTPUpstreamProfileDefault, false, false)
				} else {
					entry, err = upstream.getClientEntryWithTLS("", id, 3, profile, service.HTTPUpstreamProfileDefault, false, false)
				}
				require.NoError(t, err)
				entry.client = server.Client()
			}
			start := func(id int64, path string) *http.Response {
				req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+path, nil)
				require.NoError(t, err)
				resp, err := upstream.DoWithTLS(req, "", id, 3, profile)
				require.NoError(t, err)
				t.Cleanup(func() { _ = resp.Body.Close() })
				initial := make([]byte, len("data: partial\n\n"))
				_, err = io.ReadFull(resp.Body, initial)
				require.NoError(t, err)
				return resp
			}
			a, b := start(accountID, "/a"), start(accountID, "/b")
			other := start(accountID+100, "/other")
			done := make(chan error, 2)
			for _, resp := range []*http.Response{a, b} {
				go func() { _, err := io.ReadAll(resp.Body); done <- err }()
			}
			require.Positive(t, accountrequest.Default.Cancel(accountID, time.Now()))
			for range 2 {
				select {
				case err := <-done:
					require.ErrorIs(t, err, accountrequest.ErrQualityDegraded)
				case <-time.After(time.Second):
					t.Fatal("quality interrupt left a response body blocked")
				}
			}
			close(finishOther)
			raw, err := io.ReadAll(other.Body)
			require.NoError(t, err)
			require.Equal(t, "data: complete\n\n", string(raw))
			require.NoError(t, a.Body.Close())
			require.NoError(t, b.Body.Close())
			require.NoError(t, other.Body.Close())
			require.Zero(t, accountrequest.Default.Cancel(accountID+100, time.Now()))
		})
	}
}
