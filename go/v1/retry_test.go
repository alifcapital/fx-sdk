package v1

import (
	"errors"
	"fmt"
	"io"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestIsStreamRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"server EOF", io.EOF, true},
		{"wrapped EOF", fmt.Errorf("recv: %w", io.EOF), true},
		{"load balancer idle reset", status.Error(codes.Internal,
			"stream terminated by RST_STREAM with error code: PROTOCOL_ERROR"), true},
		{"unavailable", status.Error(codes.Unavailable, "connection refused"), true},
		{"resource exhausted", status.Error(codes.ResourceExhausted, "busy"), true},
		{"unknown", status.Error(codes.Unknown, "?"), true},
		{"non-grpc error", errors.New("boom"), true},
		{"key switched off", status.Error(codes.Unauthenticated, "bad key"), false},
		{"wrong certificate", status.Error(codes.PermissionDenied, "cn mismatch"), false},
		{"bad request", status.Error(codes.InvalidArgument, "partner_id"), false},
		{"old core", status.Error(codes.Unimplemented, "no method"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isStreamRetryable(tt.err); got != tt.want {
				t.Errorf("isStreamRetryable(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
