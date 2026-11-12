package v1

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/alifcapital/fx-sdk/go/forexv1"
	"google.golang.org/grpc"
)

// pagingCore answers GetTrades the way a Core does: total trades, handed out
// pageSize at a time behind a token. pageSize of zero is a Core that predates
// paging — it ignores the page fields and returns everything at once.
type pagingCore struct {
	forexv1.TradeServiceClient
	total    int
	pageSize int
	stuck    bool // always answers with the same token
	requests []*forexv1.GetTradesRequest
}

func (p *pagingCore) GetTrades(_ context.Context, req *forexv1.GetTradesRequest, _ ...grpc.CallOption) (*forexv1.GetTradesResponse, error) {
	p.requests = append(p.requests, req)
	start := 0
	if tok := req.GetPageToken(); tok != "" {
		start, _ = strconv.Atoi(tok)
	}
	end := p.total
	if p.pageSize > 0 {
		end = min(start+p.pageSize, p.total)
	}
	resp := &forexv1.GetTradesResponse{}
	for i := start; i < end; i++ {
		id := int64(i + 1)
		resp.Trades = append(resp.Trades, &forexv1.TradeResponse{Id: &id})
	}
	if p.stuck {
		tok := "0"
		resp.NextPageToken = &tok
	} else if end < p.total {
		tok := strconv.Itoa(end)
		resp.NextPageToken = &tok
	}
	return resp, nil
}

func TestGetTradesRawWalksEveryPage(t *testing.T) {
	core := &pagingCore{total: 2500, pageSize: 1000}
	c := &Client{trade: core}

	trades, err := c.getTradesRaw(context.Background(), "p1", "2026-11-11", "a", "b", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(trades) != 2500 {
		t.Fatalf("trades = %d, want 2500", len(trades))
	}
	for i, tr := range trades {
		if tr.GetId() != int64(i+1) {
			t.Fatalf("trade %d has id %d: a page was repeated or skipped", i, tr.GetId())
		}
	}
	if len(core.requests) != 3 {
		t.Fatalf("requests = %d, want 3", len(core.requests))
	}
	// Every page repeats the window and asks for the Core's maximum page.
	for i, req := range core.requests {
		if req.GetPageSize() != tradesPageSize || req.GetTradingDay() != "2026-11-11" ||
			req.GetPartnerId() != "p1" || !req.GetIsReconciliation() {
			t.Errorf("request %d = %v", i, req)
		}
	}
	if core.requests[0].PageToken != nil || core.requests[2].GetPageToken() != "2000" {
		t.Errorf("tokens sent: first %v, third %q", core.requests[0].PageToken, core.requests[2].GetPageToken())
	}
}

// Against a Core that knows nothing about paging the call is one round trip
// returning everything, exactly as before.
func TestGetTradesRawAgainstACoreWithoutPaging(t *testing.T) {
	core := &pagingCore{total: 2500}
	c := &Client{trade: core}

	trades, err := c.getTradesRaw(context.Background(), "p1", "2026-11-11", "a", "b", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(trades) != 2500 || len(core.requests) != 1 {
		t.Errorf("trades = %d in %d request(s), want 2500 in 1", len(trades), len(core.requests))
	}
}

// A token that never advances must end in an error, not in a loop and not in
// a partial list that reads as complete.
func TestGetTradesRawStopsWhenTheTokenDoesNotAdvance(t *testing.T) {
	core := &pagingCore{total: 10, pageSize: 5, stuck: true}
	c := &Client{trade: core}

	trades, err := c.getTradesRaw(context.Background(), "p1", "2026-11-11", "a", "b", false)
	if err == nil || !strings.Contains(err.Error(), "paging did not finish") {
		t.Fatalf("err = %v, want a paging error", err)
	}
	if trades != nil {
		t.Errorf("returned %d trades alongside the error", len(trades))
	}
	if len(core.requests) > 3 {
		t.Errorf("made %d requests before giving up", len(core.requests))
	}
}
