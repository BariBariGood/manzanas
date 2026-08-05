package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/proto"
)

// fakeImages records image-store calls for handler tests.
type fakeImages struct {
	built      *proto.ImageBuildRequest
	stampID    string
	stampCount int
	stampPref  string
	deletedID  string
	imgs       []proto.ImageInfo
	err        error
}

func (f *fakeImages) Build(ctx context.Context, req proto.ImageBuildRequest) (proto.ImageInfo, error) {
	f.built = &req
	return proto.ImageInfo{ID: "img_test", DeviceType: req.DeviceType, Runtime: req.Runtime}, f.err
}

func (f *fakeImages) List(ctx context.Context) ([]proto.ImageInfo, error) {
	return f.imgs, f.err
}

func (f *fakeImages) Stamp(ctx context.Context, id string, count int, prefix string) (proto.ImageInfo, []proto.StampedSim, error) {
	f.stampID, f.stampCount, f.stampPref = id, count, prefix
	if f.err != nil {
		return proto.ImageInfo{}, nil, f.err
	}
	out := make([]proto.StampedSim, count)
	for i := range out {
		out[i] = proto.StampedSim{UDID: "AA00", Name: prefix}
	}
	return proto.ImageInfo{ID: "img_test"}, out, nil
}

func (f *fakeImages) Delete(ctx context.Context, id string) error {
	f.deletedID = id
	return f.err
}

func newImagesTestServer(t *testing.T, imgs *fakeImages) *httptest.Server {
	t.Helper()
	srv := New(registry.NewMock(), nil, nil)
	if imgs != nil {
		srv.SetImages(imgs)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestImagesRoutes(t *testing.T) {
	f := &fakeImages{imgs: []proto.ImageInfo{{ID: "img_a"}}}
	ts := newImagesTestServer(t, f)

	// Build.
	resp, err := http.Post(ts.URL+"/v0/images/build", "application/json",
		strings.NewReader(`{"device_type":"iPhone 17","runtime":"iOS 26.5"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("build status = %d", resp.StatusCode)
	}
	resp.Body.Close()
	if f.built == nil || f.built.DeviceType != "iPhone 17" {
		t.Fatalf("build req not forwarded: %+v", f.built)
	}

	// List.
	resp, err = http.Get(ts.URL + "/v0/images")
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Images []proto.ImageInfo `json:"images"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(list.Images) != 1 || list.Images[0].ID != "img_a" {
		t.Fatalf("list = %+v", list)
	}

	// Stamp.
	resp, err = http.Post(ts.URL+"/v0/images/img_a/stamp", "application/json",
		strings.NewReader(`{"count":2,"name_prefix":"qa"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("stamp status = %d", resp.StatusCode)
	}
	var res proto.ImageStampResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// The result carries the resolved image ID, not the raw request value.
	if !res.OK || res.ImageID != "img_test" || len(res.Created) != 2 {
		t.Fatalf("stamp result = %+v", res)
	}
	if f.stampID != "img_a" || f.stampCount != 2 || f.stampPref != "qa" {
		t.Fatalf("stamp args = %q %d %q", f.stampID, f.stampCount, f.stampPref)
	}

	// Delete.
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/v0/images/img_a", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
	resp.Body.Close()
	if f.deletedID != "img_a" {
		t.Fatalf("deleted = %q", f.deletedID)
	}
}

func TestImagesNotImplementedWithoutStore(t *testing.T) {
	ts := newImagesTestServer(t, nil)
	resp, err := http.Get(ts.URL + "/v0/images")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var e proto.Error
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatal(err)
	}
	if e.Code != proto.ErrNotImplemented {
		t.Fatalf("code = %q", e.Code)
	}
}
