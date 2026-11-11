package bpsws

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

type secretNetworkError struct {
	timeout, temporary bool
	cause              error
}

func (e secretNetworkError) Error() string {
	return "https://user:proxy-secret@host.invalid/" + testBearer
}
func (e secretNetworkError) Timeout() bool   { return e.timeout }
func (e secretNetworkError) Temporary() bool { return e.temporary }
func (e secretNetworkError) Unwrap() error   { return e.cause }

func TestSanitizedNetworkErrorsKeepClassificationAndOnlySafeCauses(t *testing.T) {
	for _, phase := range []string{"dial", "write"} {
		for _, cause := range []error{nil, io.EOF, io.ErrUnexpectedEOF, syscall.ECONNREFUSED, syscall.ECONNRESET, context.Canceled, context.DeadlineExceeded} {
			for _, timeout := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/cause=%v/timeout=%t", phase, cause, timeout), func(t *testing.T) {
					ctx := testContext(t)
					raw := fmt.Errorf("sensitive outer %s: %w", testBearer, secretNetworkError{timeout: timeout, temporary: !timeout, cause: cause})
					var client *http.Client
					if phase == "dial" {
						client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, raw })}
					} else {
						socket := &faultSocket{closed: make(chan struct{}), writeErr: raw}
						client = mockUpgrade(socket, "responses")
					}
					_, err := Open(ctx, client, preparedRequest(t, ctx, "https://host.invalid/responses"), 0)
					require.Error(t, err)
					var network net.Error
					require.ErrorAs(t, err, &network, "network failures must continue to count in priority statistics")
					// The standard deadline sentinel is itself a timeout/temporary
					// net.Error, even when the outer custom error's flags are false.
					require.Equal(t, timeout || cause == context.DeadlineExceeded, network.Timeout())
					//nolint:staticcheck // Verifies compatibility with legacy net.Error callers.
					require.Equal(t, !timeout || cause == context.DeadlineExceeded, network.Temporary())
					if cause != nil {
						require.ErrorIs(t, err, cause)
					}
					for node := err; node != nil; node = errors.Unwrap(node) {
						for _, secret := range []string{testBearer, "proxy-secret", "host.invalid", "sensitive outer"} {
							require.NotContains(t, node.Error(), secret)
						}
					}
				})
			}
		}
	}
}
