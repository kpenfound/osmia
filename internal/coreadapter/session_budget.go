package coreadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

var ErrSessionCostCap = errors.New("per-session cost cap reached")

type costMonitorKey struct{}

// TODO: Remove this stream-cost monitor when busybees/core enforces a
// per-session USD cap and returns the observed partial cost on cancellation.
type costMonitor struct {
	mu      sync.Mutex
	line    []byte
	backend string
	limit   float64
	spent   float64
	known   bool
	reached bool
	cancel  context.CancelCauseFunc
}

func (m *costMonitor) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.line = append(m.line, p...)
	for {
		i := bytes.IndexByte(m.line, '\n')
		if i < 0 {
			break
		}
		m.observe(m.line[:i])
		m.line = m.line[i+1:]
	}
	return len(p), nil
}

func (m *costMonitor) observe(line []byte) {
	var event struct {
		Type         string  `json:"type"`
		TotalCostUSD float64 `json:"total_cost_usd"`
		Part         struct {
			Cost float64 `json:"cost"`
		} `json:"part"`
	}
	if json.Unmarshal(line, &event) != nil {
		return
	}
	switch {
	case m.backend == "claude" && event.Type == "result":
		m.spent, m.known = event.TotalCostUSD, true
	case m.backend == "opencode" && event.Type == "step_finish":
		m.spent += event.Part.Cost
		m.known = true
	default:
		return
	}
	if m.known && m.spent >= m.limit && !m.reached {
		m.reached = true
		m.cancel(ErrSessionCostCap)
	}
}

func (m *costMonitor) snapshot() (float64, bool, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.spent, m.known, m.reached
}

func (m *costMonitor) stream(original io.Writer) io.Writer {
	if original == nil {
		return m
	}
	return io.MultiWriter(original, m)
}

func sessionCapError(limit float64) error {
	return fmt.Errorf("%w: budget.per_session %.2f USD", ErrSessionCostCap, limit)
}
