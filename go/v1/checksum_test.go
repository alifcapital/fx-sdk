package v1

import (
	"testing"

	"github.com/alifcapital/fx-sdk/go/forexv1"
)

func resp(hashCheck int64, hashV2, tradeCount *int64) *forexv1.ReconciliationResponse {
	return &forexv1.ReconciliationResponse{
		HashCheck:  &hashCheck,
		HashV2:     hashV2,
		TradeCount: tradeCount,
	}
}

func TestCompareChecksums(t *testing.T) {
	i64 := func(v int64) *int64 { return &v }

	tests := []struct {
		name         string
		localV1      int64
		localV2      int64
		localTrades  int64
		resp         *forexv1.ReconciliationResponse
		wantVersion  int
		wantMatched  bool
		wantRemote   int64
		wantRemoteNo int64
	}{
		{
			name:    "tuple checksum and count both agree",
			localV1: 111, localV2: 222, localTrades: 7,
			resp:        resp(111, i64(222), i64(7)),
			wantVersion: 2, wantMatched: true, wantRemote: 222, wantRemoteNo: 7,
		},
		{
			// The case the count exists for: contributions that cancel hash to
			// the same zero an empty window does.
			name:    "hash agrees but the row counts do not",
			localV1: 111, localV2: 0, localTrades: 2,
			resp:        resp(111, i64(0), i64(0)),
			wantVersion: 2, wantMatched: false, wantRemote: 0, wantRemoteNo: 0,
		},
		{
			name:    "counts agree but the hash does not",
			localV1: 111, localV2: 222, localTrades: 7,
			resp:        resp(111, i64(999), i64(7)),
			wantVersion: 2, wantMatched: false, wantRemote: 999, wantRemoteNo: 7,
		},
		{
			// An empty hour on both sides is all zeroes, and zero is a value, not
			// an absence. Reading the fields through the getters and testing for 0
			// would downgrade every quiet hour to the weaker comparison.
			name:    "an empty window on both sides still compares at version 2",
			localV1: 0, localV2: 0, localTrades: 0,
			resp:        resp(0, i64(0), i64(0)),
			wantVersion: 2, wantMatched: true, wantRemote: 0, wantRemoteNo: 0,
		},
		{
			name:    "a Core that sends neither new field",
			localV1: 111, localV2: 222, localTrades: 7,
			resp:        resp(111, nil, nil),
			wantVersion: 1, wantMatched: true, wantRemote: 111, wantRemoteNo: -1,
		},
		{
			name:    "a Core that sends the hash but no count",
			localV1: 111, localV2: 222, localTrades: 7,
			resp:        resp(111, i64(222), nil),
			wantVersion: 1, wantMatched: true, wantRemote: 111, wantRemoteNo: -1,
		},
		{
			name:    "version 1 disagreeing",
			localV1: 111, localV2: 222, localTrades: 7,
			resp:        resp(555, nil, nil),
			wantVersion: 1, wantMatched: false, wantRemote: 555, wantRemoteNo: -1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := compareChecksums(tt.localV1, tt.localV2, tt.localTrades, tt.resp)
			if got.version != tt.wantVersion {
				t.Errorf("version = %d, want %d", got.version, tt.wantVersion)
			}
			if got.matched != tt.wantMatched {
				t.Errorf("matched = %v, want %v", got.matched, tt.wantMatched)
			}
			if got.remote != tt.wantRemote {
				t.Errorf("remote = %d, want %d", got.remote, tt.wantRemote)
			}
			if got.remoteTrades != tt.wantRemoteNo {
				t.Errorf("remoteTrades = %d, want %d", got.remoteTrades, tt.wantRemoteNo)
			}
			wantLocal := tt.localV1
			if tt.wantVersion == 2 {
				wantLocal = tt.localV2
			}
			if got.local != wantLocal {
				t.Errorf("local = %d, want %d", got.local, wantLocal)
			}
		})
	}
}
