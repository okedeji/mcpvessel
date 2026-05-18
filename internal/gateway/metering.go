package gateway

import (
	"sync"
	"sync/atomic"
)

type TokenMeter struct {
	mu    sync.RWMutex
	cages map[string]*cageMetrics
}

type cageMetrics struct {
	assessmentID string
	inputTokens  atomic.Int64
	outputTokens atomic.Int64
}

func NewTokenMeter() *TokenMeter {
	return &TokenMeter{cages: make(map[string]*cageMetrics)}
}

func (m *TokenMeter) Record(cageID, assessmentID, model string, input, output int64) {
	m.mu.RLock()
	cm, ok := m.cages[cageID]
	m.mu.RUnlock()

	if !ok {
		m.mu.Lock()
		cm, ok = m.cages[cageID]
		if !ok {
			cm = &cageMetrics{assessmentID: assessmentID}
			m.cages[cageID] = cm
		}
		m.mu.Unlock()
	}

	cm.inputTokens.Add(input)
	cm.outputTokens.Add(output)
}

func (m *TokenMeter) AssessmentTokens(assessmentID string) int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var total int64
	for _, cm := range m.cages {
		if cm.assessmentID == assessmentID {
			total += cm.inputTokens.Load() + cm.outputTokens.Load()
		}
	}
	return total
}

func (m *TokenMeter) GetUsage(cageID string) TokenUsage {
	m.mu.RLock()
	cm, ok := m.cages[cageID]
	m.mu.RUnlock()

	if !ok {
		return TokenUsage{CageID: cageID}
	}

	return TokenUsage{
		CageID:       cageID,
		InputTokens:  cm.inputTokens.Load(),
		OutputTokens: cm.outputTokens.Load(),
	}
}

// SetUsage sets the cumulative token count for a cage. Called by the
// cage proxy-control vsock collector when the in-cage payload-proxy
// reports its running consumption total.
func (m *TokenMeter) SetUsage(cageID, assessmentID string, consumed int64) {
	m.mu.Lock()
	cm, ok := m.cages[cageID]
	if !ok {
		cm = &cageMetrics{assessmentID: assessmentID}
		m.cages[cageID] = cm
	}
	m.mu.Unlock()
	cm.inputTokens.Store(consumed)
	cm.outputTokens.Store(0)
}

func (m *TokenMeter) Reset(cageID string) {
	m.mu.Lock()
	delete(m.cages, cageID)
	m.mu.Unlock()
}
