package service

import (
	"context"
	"testing"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/pulls"
)

func resolutionApprovedFixture(t *testing.T, configuration productionDesignConfiguration, edge string, host pulls.Client, prepare ...func(*productionDesignFixture)) *productionDesignFixture {
	t.Helper()
	if host == nil {
		host = &fakePulls{}
	}
	options := configuration.options
	configuration.options = func(opts *Options) {
		if options != nil {
			options(opts)
		}
		opts.PullRequests = host
	}
	f := newConfiguredProductionDesignFixture(t, configuration)
	for _, setup := range prepare {
		setup(f)
	}
	f.sharedDesign().ScriptResolution(t, edge, ProjectSpecDraftTool)
	f.startOwner(t)
	packet := f.awaitPacket(t)
	_, err := f.c.Ratify(context.Background(), f.origin, packet.Packet.Revision.Spec, packet.Packet.Revision.Plan, packet.ProjectSpecRevision)
	must(t, err)
	f.awaitApproved(t)
	return f
}

func resolutionMembers(f *productionDesignFixture) []struct {
	project config.ProjectID
	stream  config.WorkstreamID
} {
	return []struct {
		project config.ProjectID
		stream  config.WorkstreamID
	}{{f.project, f.stream1}, {f.project2, f.stream2}}
}

func deliverResolutionMembers(t *testing.T, f *productionDesignFixture) []DeliveryPublication {
	t.Helper()
	return f.sharedDesign().DeliverResolutionMembers(t, publicationDocument, publicationPath)
}
