package intake

import (
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

func GroupApprovalResponse(project config.ProjectID, stream config.WorkstreamID, decision trace.GroupApproval) RatifyResponse {
	out := RatifyResponse{Project: project, Workstream: stream, Sealing: "pending", Detail: "the owner approved the workgroup; member seal preparations are pending"}
	for _, member := range decision.Members {
		out.Members = append(out.Members, member.Workstream)
		if member.Workstream == stream {
			out.Round, out.Spec, out.Plan = member.Round, member.Spec, member.Plan
		}
	}
	if decision.Complete {
		out.Sealing = "succeeded"
		out.Detail = "every pinned member seal is complete; the workgroup is ratified"
	}
	return out
}
