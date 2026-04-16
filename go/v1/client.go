package v1

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/alifcapital/fx-sdk/go/forexv1"
)

// Client wraps the FX Core gRPC services with a local orders database.
type Client struct {
	conn    *grpc.ClientConn
	db      *pgxpool.Pool
	order   forexv1.OrderServiceClient
	partner forexv1.PartnerServiceClient
	trade   forexv1.TradeServiceClient
}

type config struct {
	maxRetries int
	baseDelay  time.Duration
	maxDelay   time.Duration
	dialOpts   []grpc.DialOption
}

// Option configures the Client.
type Option func(*config)

// WithMaxRetries sets the maximum number of retries for transient gRPC errors.
// Default is 3.
func WithMaxRetries(n int) Option {
	return func(c *config) { c.maxRetries = n }
}

// WithRetryBackoff sets the base and max delays for exponential backoff.
// Default: 100ms base, 5s max.
func WithRetryBackoff(base, max time.Duration) Option {
	return func(c *config) { c.baseDelay = base; c.maxDelay = max }
}

// WithDialOptions appends additional gRPC dial options (e.g. TLS credentials).
func WithDialOptions(opts ...grpc.DialOption) Option {
	return func(c *config) { c.dialOpts = append(c.dialOpts, opts...) }
}

// New creates a new FX SDK client.
// target is the gRPC server address (e.g. "fx-core.example.com:443").
// partnerId and apiKey are sent as gRPC metadata on every request.
// db is the pgxpool connection pool for the local orders table.
func New(target string, partnerId, apiKey string, db *pgxpool.Pool, opts ...Option) (*Client, error) {
	if target == "" {
		return nil, fmt.Errorf("fx-sdk: target is required")
	}
	if ln := len(partnerId); ln != 36 {
		return nil, fmt.Errorf("fx-sdk: partner_id is invalid")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("fx-sdk: api_key is required")
	}
	sign, err := signApiKey(apiKey, partnerId)
	if err != nil {
		return nil, err
	}
	if db == nil {
		return nil, fmt.Errorf("fx-sdk: db is required")
	}

	cfg := config{
		maxRetries: 3,
		baseDelay:  100 * time.Millisecond,
		maxDelay:   5 * time.Second,
	}
	for _, o := range opts {
		o(&cfg)
	}

	md := metadata.Pairs("partner-id", partnerId, "api-key", sign)

	dialOpts := make([]grpc.DialOption, 0, len(cfg.dialOpts)+2)
	dialOpts = append(dialOpts,
		grpc.WithChainUnaryInterceptor(
			authUnaryInterceptor(md),
			retryUnaryInterceptor(cfg.maxRetries, cfg.baseDelay, cfg.maxDelay),
		),
		grpc.WithChainStreamInterceptor(
			authStreamInterceptor(md),
		),
	)
	dialOpts = append(dialOpts, cfg.dialOpts...)

	conn, err := grpc.NewClient(target, dialOpts...)
	if err != nil {
		return nil, err
	}

	return &Client{
		conn:    conn,
		db:      db,
		order:   forexv1.NewOrderServiceClient(conn),
		partner: forexv1.NewPartnerServiceClient(conn),
		trade:   forexv1.NewTradeServiceClient(conn),
	}, nil
}

func authUnaryInterceptor(md metadata.MD) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		return invoker(metadata.NewOutgoingContext(ctx, md), method, req, reply, cc, opts...)
	}
}

func authStreamInterceptor(md metadata.MD) grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		return streamer(metadata.NewOutgoingContext(ctx, md), desc, cc, method, opts...)
	}
}

// Close releases the underlying gRPC connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

func signApiKey(apiKey, partnerId string) (string, error) {
	priv, err := base64.StdEncoding.DecodeString(apiKey)
	if err != nil {
		return "", err
	}
	sign := ed25519.Sign(priv, []byte(partnerId))
	return base64.StdEncoding.EncodeToString(sign), nil
}
