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
		limit := max(maxRetries, 0)
		for attempt := range limit + 1 {
			err = invoker(ctx, method, req, reply, cc, opts...)
			if err == nil {
				return nil
			}
			if !isRetryable(err) || attempt == limit {
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
// bidirectional stream should trigger a reconnect.
//
// A subscription is expected to run forever, so this is the reverse of
// isRetryable: every error reconnects except the few that a reconnect cannot
// fix, because the Core refused this client or this request. Giving up on
// anything else leaves the partner disconnected, missing trades, until the
// process is restarted.
//
// That matters because a stream is closed by things other than the Core. A load
// balancer drops it after its idle timeout (an hour on the FX Core ALB), and
// gRPC reports the proxy's RST_STREAM as codes.Internal, "stream terminated by
// RST_STREAM with error code: PROTOCOL_ERROR". A clean io.EOF from the server
// is likewise a restart signal, not a terminal condition.
func isStreamRetryable(err error) bool {
	if errors.Is(err, io.EOF) {
		return true
	}
	st, ok := status.FromError(err)
	if !ok {
		return true
	}
	switch st.Code() {
	case codes.Unauthenticated, codes.PermissionDenied,
		codes.InvalidArgument, codes.Unimplemented:
		return false
	default:
		return true
	}
}

// backoff returns a randomized exponential backoff delay for the given attempt,
// capped at max. It doubles iteratively and stops at max, so it never overflows
// regardless of how large attempt grows (stream reconnects are unbounded).
func backoff(attempt int, base, max time.Duration) time.Duration {
	delay := base
	for i := 0; i < attempt && delay < max; i++ {
		delay <<= 1
	}
	delay = min(delay, max)
	half := delay / 2
	if half <= 0 {
		return delay
	}
	return half + time.Duration(rand.Int64N(int64(half)))
}
