package mcp

import (
	"context"
	"fmt"

	"github.com/BariBariGood/manzanas/internal/runspec"
	"github.com/BariBariGood/manzanas/proto"
)

func toolRun() Tool {
	return Tool{
		Name: "run",
		Description: "Execute a whole simulator run in ONE call from a declarative YAML run-spec: " +
			"the daemon acquires a lease on a matching target (labels/udid/runtime/device_type), " +
			"boots it, applies fixtures, installs and launches the app, executes the steps through " +
			"the normal action pipeline (tap/type/tap_element/wait_for_element/screenshot/audit/...), " +
			"captures evidence into the run journal, and releases the lease (applying its reset) — " +
			"even when a step fails. Prefer this over hand-sequencing lease_acquire/app/actions/" +
			"lease_release when you know the whole flow up front. The result reports per-step " +
			"status and includes the journal's markdown export (evidence) by default; the journal " +
			"run_id equals the returned lease_id. Sync by default (bounded by the spec's " +
			"timeouts.run_seconds, default 600); pass async=true and poll with run_status for " +
			"long runs. See docs/runs.md for the spec schema.",
		InputSchema: schema(map[string]map[string]any{
			"spec_yaml": {"type": "string", "description": "the run-spec as YAML (schema in docs/runs.md): name, target{labels,udid,runtime,device_type,fixtures,reset}, app{path,bundle_id}, steps[{action,with,...}], artifacts, timeouts"},
			"agent_id":  {"type": "string", "description": "attribution for the lease and journal; set it so runs are attributable (e.g. your session ID)"},
			"async":     {"type": "boolean", "description": "return immediately with the pending run; poll with run_status (default false)"},
		}, "spec_yaml", "agent_id"),
		Call: func(ctx context.Context, s *Server, args map[string]any) ([]map[string]any, error) {
			specYAML, err := reqStr(args, "spec_yaml")
			if err != nil {
				return nil, err
			}
			agentID, err := reqStr(args, "agent_id")
			if err != nil {
				return nil, err
			}
			async, _ := args["async"].(bool)
			spec, err := runspec.Parse([]byte(specYAML))
			if err != nil {
				return nil, err
			}
			run, err := s.client.StartRun(ctx, proto.RunRequest{
				Spec: spec, AgentID: agentID, Async: async,
			})
			if err != nil {
				return nil, err
			}
			return jsonContent(run)
		},
	}
}

func toolRunStatus() Tool {
	return Tool{
		Name: "run_status",
		Description: "Fetch a run started with the run tool (async or still executing) by its run ID. " +
			"Returns the run resource: state (pending/running/passed/failed), current stage, per-step " +
			"results, and — once finished — the journal's markdown export when the spec asked for it " +
			"(the default). A 404 means the run ID is unknown to this daemon (run resources are " +
			"in-memory; the durable evidence is the journal, keyed by the run's lease_id).",
		InputSchema: schema(map[string]map[string]any{
			"run_id": {"type": "string", "description": "the run ID returned by the run tool (run_...)"},
		}, "run_id"),
		Call: func(ctx context.Context, s *Server, args map[string]any) ([]map[string]any, error) {
			runID, err := reqStr(args, "run_id")
			if err != nil {
				return nil, err
			}
			run, err := s.client.GetRun(ctx, runID)
			if err != nil {
				return nil, fmt.Errorf("run_status: %w", err)
			}
			return jsonContent(run)
		},
	}
}
