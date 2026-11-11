package bpsws

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
)

// Never retain the original error: even an Unwrap chain can expose bearer
// subprotocols or proxy URLs to structured logging. Preserve only safe standard
// sentinels and the network classification used by account error statistics.
func safeError(ctx context.Context, err error, message string) error {
	var cause error
	switch {
	case ctx.Err() == context.Canceled || errors.Is(err, context.Canceled):
		cause = context.Canceled
	case ctx.Err() == context.DeadlineExceeded || errors.Is(err, context.DeadlineExceeded):
		cause = context.DeadlineExceeded
	default:
		for _, sentinel := range []error{syscall.ECONNREFUSED, syscall.ECONNRESET, io.ErrUnexpectedEOF, io.EOF, net.ErrClosed} {
			if errors.Is(err, sentinel) {
				cause = sentinel
				break
			}
		}
	}
	var isNetwork, timeout, temporary bool
	// url.Error's methods only inspect its immediate Err, so an intervening
	// fmt.Errorf wrapper can hide an underlying network timeout. Inspect every
	// layer, retaining positive classification without retaining the errors.
	for node := err; node != nil; node = errors.Unwrap(node) {
		if network, ok := node.(net.Error); ok {
			isNetwork = true
			timeout = timeout || network.Timeout()
			temporary = temporary || network.Temporary()
		}
	}
	if isNetwork {
		return &safeNetworkError{message: message, timeout: timeout, temporary: temporary, cause: cause}
	}
	if cause != nil {
		return fmt.Errorf("%s: %w", message, cause)
	}
	return errors.New(message)
}

type safeNetworkError struct {
	message            string
	timeout, temporary bool
	cause              error
}

func (e *safeNetworkError) Error() string   { return e.message }
func (e *safeNetworkError) Timeout() bool   { return e.timeout }
func (e *safeNetworkError) Temporary() bool { return e.temporary }
func (e *safeNetworkError) Unwrap() error   { return e.cause }
