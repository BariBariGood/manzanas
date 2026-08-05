package server

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BariBariGood/manzanas/internal/lease"
	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/proto"
)

func adviseServer(t *testing.T) http.Handler {
	t.Helper()
	reg := registry.NewMock()
	s := New(reg, nil, slog.Default())
	m := lease.New(reg, s.EventSink())
	t.Cleanup(m.Close)
	s.SetLeases(m)
	return s.Handler()
}

func postAdvise(t *testing.T, h http.Handler, req proto.PoolAdviseRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v0/pool/advise", bytes.NewReader(body)))
	return rec
}

func TestAdviseRecordedOnStatus(t *testing.T) {
	h := adviseServer(t)
	rec := postAdvise(t, h, proto.PoolAdviseRequest{
		Source:        "broker",
		WindowSeconds: 600,
		Classes: []proto.PoolClassAdvice{
			{Labels: []string{"ios26"}, Action: proto.AdviceGrow, ColdPlacements: 5},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("advise: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Accepted bool `json:"accepted"`
		Acted    bool `json:"acted"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Accepted || resp.Acted {
		t.Fatalf("advise must be recorded but never acted on: %+v", resp)
	}

	st := getStatus(t, h)
	if st.PoolAdvice == nil || st.PoolAdvice.Source != "broker" ||
		st.PoolAdvice.WindowSeconds != 600 || len(st.PoolAdvice.Classes) != 1 {
		t.Fatalf("status pool_advice: %+v", st.PoolAdvice)
	}
	if st.PoolAdvice.ReceivedAt.IsZero() {
		t.Fatal("received_at not set")
	}
}

func TestAdviseValidation(t *testing.T) {
	h := adviseServer(t)
	// Unknown action.
	rec := postAdvise(t, h, proto.PoolAdviseRequest{
		Classes: []proto.PoolClassAdvice{{Labels: []string{"x"}, Action: "explode"}},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown action: %d", rec.Code)
	}
	// Grow without a label class.
	rec = postAdvise(t, h, proto.PoolAdviseRequest{
		Classes: []proto.PoolClassAdvice{{Action: proto.AdviceGrow}},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("grow without labels: %d", rec.Code)
	}
	// Rejected advice must not be recorded.
	if st := getStatus(t, h); st.PoolAdvice != nil {
		t.Fatalf("rejected advice recorded: %+v", st.PoolAdvice)
	}
}
