package client

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/coreadapter"
	"github.com/kpenfound/osmia/internal/doctor"
)

// The Dagger settings of this machine. A client has no configuration of its
// own: a mason's Dagger runs on Dagger Cloud when the machine holds a compute
// token, and otherwise on the engine the machine's Dagger CLI is pointed at.
const (
	daggerCloudToken = coreadapter.DaggerCloudTokenEnvironment
	daggerEngineHost = "_EXPERIMENTAL_DAGGER_RUNNER_HOST"
)

// toolsFor is what one profile of the handout takes to run on this machine:
// the backend, and for every role whose chain holds the profile the sandbox
// and skills the role runs with, and Dagger when the profile is the main
// profile of a role that runs it.
func toolsFor(h api.ProfileHandout, profile api.ProfileSpec) doctor.Tools {
	need := doctor.Tools{Agent: profile.Backend}
	for _, role := range h.Roles {
		if !slices.Contains(role.Chain, profile.Name) {
			continue
		}
		switch role.Sandbox {
		case agent.SandboxContainer:
			need.Container = true
		case agent.SandboxSbx:
			need.Sbx = true
			need.Dagger = need.Dagger || role.Dagger != nil && role.Profile == profile.Name
		}
		if profile.Backend == "" || profile.Backend == agent.AgentClaude || profile.Backend == agent.AgentCodex {
			for _, ref := range role.Skills {
				if !slices.Contains(need.Skills, ref) {
					need.Skills = append(need.Skills, ref)
				}
			}
		}
	}
	return need
}

// memo runs each command once per capability pass, so a tool every profile
// needs is asked once.
type memo struct {
	run func(ctx context.Context, path string, args ...string) (string, error)
	mu  sync.Mutex
	got map[string]memoed
}

type memoed struct {
	out string
	err error
}

func (m *memo) command(ctx context.Context, path string, args ...string) (string, error) {
	key := path + "\x00" + strings.Join(args, "\x00")
	m.mu.Lock()
	got, ok := m.got[key]
	m.mu.Unlock()
	if ok {
		return got.out, got.err
	}
	out, err := m.run(ctx, path, args...)
	m.mu.Lock()
	m.got[key] = memoed{out, err}
	m.mu.Unlock()
	return out, err
}

// check reports which profiles of the handout this machine can run, with the
// fix for everything it lacks. It runs the tool checks osmia doctor runs.
// Provider login is not checked: a missing login shows up as a failed turn.
func (c *Client) check(ctx context.Context, h api.ProfileHandout) api.CapabilityReport {
	m := &memo{run: c.command, got: map[string]memoed{}}
	options := doctor.Options{LookPath: c.lookPath, Command: m.command}
	report := api.CapabilityReport{Revision: h.Revision, Profiles: []api.ProfileCapability{}}
	for _, profile := range h.Profiles {
		need := toolsFor(h, profile)
		capability := api.ProfileCapability{Profile: profile.Name, Runnable: true}
		checks := doctor.CheckTools(ctx, options, need)
		if need.Dagger {
			checks = append(checks, c.daggerSetting())
		}
		for _, check := range checks {
			if check.Status == doctor.Pass {
				continue
			}
			capability.Problems = append(capability.Problems, api.ProfileProblem{Check: check.Name, Detail: check.Detail, Fix: check.Remediation})
			if check.Status == doctor.Fail {
				capability.Runnable = false
			}
		}
		report.Profiles = append(report.Profiles, capability)
	}
	return report
}

// daggerSetting checks that the machine says where a mason's Dagger runs.
func (c *Client) daggerSetting() doctor.Check {
	check := doctor.Check{Group: doctor.GroupToolchain, Name: "dagger engine", Status: doctor.Pass}
	switch {
	case c.getenv(daggerCloudToken) != "":
		check.Detail = "Dagger Cloud, with the token in " + daggerCloudToken
	case c.getenv(daggerEngineHost) != "":
		check.Detail = "the engine in " + daggerEngineHost
	default:
		check.Status = doctor.Fail
		check.Detail = "neither " + daggerCloudToken + " nor " + daggerEngineHost + " is set"
		check.Remediation = "set " + daggerCloudToken + " to a Dagger Cloud compute token, or " + daggerEngineHost + " to the engine a mason's sandbox should use, in the environment osmia client runs in"
	}
	return check
}

func (c *Client) describe(report api.CapabilityReport) {
	for _, p := range report.Profiles {
		if p.Runnable {
			c.log.Info("profile can run here", "profile", p.Profile)
		} else {
			c.log.Warn("profile cannot run here", "profile", p.Profile)
		}
		for _, problem := range p.Problems {
			c.log.Warn("  "+problem.Check+": "+problem.Detail, "profile", p.Profile, "fix", problem.Fix)
		}
	}
}

// checkLoop checks the latest handout whenever one arrives or the client
// connects, and reports it on the lease it holds.
func (c *Client) checkLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.recheck:
		}
		for ctx.Err() == nil {
			c.mu.Lock()
			h, lease, connected := c.handout, c.lease, c.connected
			c.mu.Unlock()
			if lease == "" || !connected {
				break
			}
			report := c.check(ctx, h)
			if ctx.Err() != nil {
				return
			}
			c.mu.Lock()
			stale := c.handout.Revision != h.Revision || c.lease != lease
			c.mu.Unlock()
			if stale {
				continue
			}
			c.describe(report)
			if err := c.post(ctx, api.ClientRoute(lease, "capabilities"), report, &api.CapabilityResponse{}, callTimeout); err != nil {
				c.log.Warn("cannot report what this client can run", "error", err)
				time.AfterFunc(c.retryEvery, c.wakeCheck)
			}
			break
		}
	}
}

func (c *Client) wakeCheck() {
	select {
	case c.recheck <- struct{}{}:
	default:
	}
}
