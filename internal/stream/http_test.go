package stream

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/BariBariGood/manzanas/proto"
)

func testServer(t *testing.T, m *Manager) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /streams/{id}/mjpeg", func(w http.ResponseWriter, r *http.Request) {
		m.ServeMJPEG(w, r, r.PathValue("id"))
	})
	mux.HandleFunc("GET /streams/{id}/ws", func(w http.ResponseWriter, r *http.Request) {
		m.ServeWS(w, r, r.PathValue("id"))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func readMJPEGPart(t *testing.T, mr *multipart.Reader) []byte {
	t.Helper()
	part, err := mr.NextPart()
	if err != nil {
		t.Fatalf("NextPart: %v", err)
	}
	defer part.Close()
	if ct := part.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("part Content-Type = %q, want image/jpeg", ct)
	}
	data, err := io.ReadAll(part)
	if err != nil {
		t.Fatalf("read part: %v", err)
	}
	return data
}

func TestServeMJPEG(t *testing.T) {
	m := testManager(t, Config{DefaultFPS: 30})
	offer, err := m.Open(context.Background(), "UDID-1", proto.StreamRequest{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ts := testServer(t, m)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/streams/"+offer.StreamID+"/mjpeg", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET mjpeg: %v", err)
	}
	defer resp.Body.Close()
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "multipart/x-mixed-replace") {
		t.Fatalf("Content-Type = %q", ct)
	}
	boundary := strings.TrimPrefix(ct, "multipart/x-mixed-replace; boundary=")
	mr := multipart.NewReader(bufio.NewReader(resp.Body), boundary)
	for i := 0; i < 3; i++ {
		frame := readMJPEGPart(t, mr)
		if len(frame) < 2 || frame[0] != 0xFF || frame[1] != 0xD8 {
			t.Fatalf("part %d is not a JPEG", i)
		}
	}
}

func TestServeMJPEGUnknownStream(t *testing.T) {
	m := testManager(t, Config{})
	ts := testServer(t, m)
	resp, err := http.Get(ts.URL + "/streams/stm_nope/mjpeg")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var e proto.Error
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if e.Code != proto.ErrNotFound || e.Message == "" {
		t.Errorf("error = %+v, want code %q", e, proto.ErrNotFound)
	}
}

func TestServeMJPEGViewerLimitJSONEnvelope(t *testing.T) {
	m := testManager(t, Config{DefaultFPS: 30, MaxViewers: 1})
	offer, _ := m.Open(context.Background(), "UDID-1", proto.StreamRequest{})
	h, _ := m.get(offer.StreamID)
	_, detach, err := h.attach()
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer detach()
	ts := testServer(t, m)

	resp, err := http.Get(ts.URL + "/streams/" + offer.StreamID + "/mjpeg")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	var e proto.Error
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if e.Code != proto.ErrViewerLimit || e.Message == "" {
		t.Errorf("error = %+v, want code %q", e, proto.ErrViewerLimit)
	}
}

func TestServeWSDeliversBinaryJPEGFrames(t *testing.T) {
	m := testManager(t, Config{DefaultFPS: 30})
	offer, err := m.Open(context.Background(), "UDID-1", proto.StreamRequest{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ts := testServer(t, m)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/streams/" + offer.StreamID + "/ws"
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	for i := 0; i < 3; i++ {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("ws read %d: %v", i, err)
		}
		if typ != websocket.MessageBinary {
			t.Fatalf("message type = %v, want binary", typ)
		}
		if len(data) < 2 || data[0] != 0xFF || data[1] != 0xD8 {
			t.Fatalf("frame %d is not a JPEG", i)
		}
	}
}

func TestConcurrentMJPEGAndWSViewers(t *testing.T) {
	m := testManager(t, Config{DefaultFPS: 30})
	offer, _ := m.Open(context.Background(), "UDID-1", proto.StreamRequest{})
	ts := testServer(t, m)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/streams/"+offer.StreamID+"/mjpeg", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET mjpeg: %v", err)
	}
	defer resp.Body.Close()

	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/streams/" + offer.StreamID + "/ws"
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	boundary := strings.TrimPrefix(resp.Header.Get("Content-Type"), "multipart/x-mixed-replace; boundary=")
	mr := multipart.NewReader(bufio.NewReader(resp.Body), boundary)
	readMJPEGPart(t, mr)
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatalf("ws read: %v", err)
	}
	if n := m.ViewerCount(offer.StreamID); n != 2 {
		t.Errorf("viewer count = %d, want 2", n)
	}
}
