package v1

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func retryUnaryInterceptor(maxRetries int, baseDelay, maxDelay time.Duration) grpc.UnaryClientInterceptor {
	return func(
		ctx context.Context,
		method string,
		req, reply any,
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		var err error
		for attempt := range maxRetries + 1 {
			err = invoker(ctx, method, req, reply, cc, opts...)
			if err == nil {
				return nil
			}
			if !isRetryable(err) || attempt == maxRetries {
				return err
			}
			delay := backoff(attempt, baseDelay, maxDelay)
			t := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				t.Stop()
				return ctx.Err()
			case <-t.C:
			}
		}
		return err
	}
}

func isRetryable(err error) bool {
	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	switch st.Code() {
	case codes.Unavailable, codes.ResourceExhausted:
		return true
	default:
		return false
	}
}

// isStreamRetryable reports whether an error from a long-lived server or
// bidirectional stream should trigger a reconnect. A clean io.EOF from the
// server is treated as a restart signal rather than a terminal condition,
// since subscriptions like SubscribeOrderEvents are expected to run forever.
func isStreamRetryable(err error) bool {
	if errors.Is(err, io.EOF) {
		return true
	}
	return isRetryable(err)
}

func backoff(attempt int, base, max time.Duration) time.Duration {
	delay := base << attempt
	delay = min(delay, max)
	half := delay / 2
	if half <= 0 {
		return delay
	}
	return half + time.Duration(rand.Int64N(int64(half)))
}
