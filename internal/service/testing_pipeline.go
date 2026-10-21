package service

import (
	"context"
	"sync/atomic"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// TestScheduleHook binds a fixture observation around a production schedule hook.
type TestScheduleHook struct {
	Name string
	Pass func(context.Context) error
}
type TestStages struct {
	Hooks []TestScheduleHook
	Polls []TestScheduleHook
	value stages
}
type TestStagePointer struct{ p *atomic.Pointer[stages] }

func (p TestStagePointer) Load() *TestStages              { return testStages(p.p.Load()) }
func (p TestStagePointer) Store(v *TestStages)            { p.p.Store(v.stages()) }
func (p TestStagePointer) Swap(v *TestStages) *TestStages { return testStages(p.p.Swap(v.stages())) }
func testStages(s *stages) *TestStages {
	if s == nil {
		return nil
	}
	v := &TestStages{value: *s}
	for _, h := range s.hooks {
		v.Hooks = append(v.Hooks, TestScheduleHook{h.name, h.pass})
	}
	for _, h := range s.polls {
		v.Polls = append(v.Polls, TestScheduleHook{h.name, h.pass})
	}
	return v
}
func (s *TestStages) stages() *stages {
	if s == nil {
		return nil
	}
	v := s.value
	v.hooks = nil
	for _, h := range s.Hooks {
		v.hooks = append(v.hooks, scheduleHook{h.Name, h.Pass})
	}
	v.polls = nil
	for _, h := range s.Polls {
		v.polls = append(v.polls, scheduleHook{h.Name, h.Pass})
	}
	return &v
}

type TestPipeline struct {
	Current, Next TestStagePointer
	value         *pipeline
}

func testPipeline(p *pipeline) *TestPipeline {
	if p == nil {
		return nil
	}
	return &TestPipeline{Current: TestStagePointer{&p.current}, Next: TestStagePointer{&p.next}, value: p}
}
func NewPipelineForTest() *TestPipeline { return testPipeline(&pipeline{}) }
func (a TestAccess) Pipeline(id config.ProjectID) *TestPipeline {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	for _, p := range a.s.projects {
		if p.id == id {
			return testPipeline(p.pipeline)
		}
	}
	return nil
}
func (a TestAccess) SetPipeline(id config.ProjectID, p *TestPipeline) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	for _, active := range a.s.projects {
		if active.id == id {
			active.pipeline = p.value
			return
		}
	}
}
func (a TestAccess) Stages(cfg *config.Config, repo *trace.Repository) (*TestStages, error) {
	st, err := a.s.stages(cfg, repo)
	return testStages(st), err
}
