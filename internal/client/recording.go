package client

import (
	"context"
	"net/http"
	"net/url"

	"github.com/BariBariGood/manzanas/proto"
)

// StartRecording calls POST /v0/targets/{udid}/recording.
func (c *Client) StartRecording(ctx context.Context, udid string, req proto.RecordingRequest) (proto.Recording, error) {
	var rec proto.Recording
	err := c.do(ctx, http.MethodPost, "/v0/targets/"+url.PathEscape(udid)+"/recording", req, &rec)
	return rec, err
}

// StopRecording calls POST /v0/targets/{udid}/recording/stop.
func (c *Client) StopRecording(ctx context.Context, udid, leaseID string) (proto.RecordingStopResult, error) {
	var res proto.RecordingStopResult
	err := c.do(ctx, http.MethodPost, "/v0/targets/"+url.PathEscape(udid)+"/recording/stop",
		proto.RecordingStopRequest{LeaseID: leaseID}, &res)
	return res, err
}
