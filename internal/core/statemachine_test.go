package core

import (
	"errors"
	"testing"

	"servermonitor/internal/protocol"
)

func TestStateMachine_LegalTransitions(t *testing.T) {
	legal := []struct{ from, to protocol.InstanceState }{
		{protocol.InstanceStateCreated, protocol.InstanceStateStarting},
		{protocol.InstanceStateStarting, protocol.InstanceStateRunning},
		{protocol.InstanceStateStarting, protocol.InstanceStateError},   // 就緒逾時
		{protocol.InstanceStateStarting, protocol.InstanceStateCrashed}, // 啟動途中 die
		{protocol.InstanceStateRunning, protocol.InstanceStateStopping},
		{protocol.InstanceStateStopping, protocol.InstanceStateStopped},
		{protocol.InstanceStateRunning, protocol.InstanceStateCrashed},
		{protocol.InstanceStateStopped, protocol.InstanceStateStarting},
		{protocol.InstanceStateCrashed, protocol.InstanceStateStarting}, // 重試(T11)
		{protocol.InstanceStateCrashed, protocol.InstanceStateError},    // RESTART_GIVEUP(T11)
		{protocol.InstanceStateRunning, protocol.InstanceStateBackingUp},
		{protocol.InstanceStateRunning, protocol.InstanceStateOffline},
		{protocol.InstanceStateRunning, protocol.InstanceStateRunning}, // 同狀態 no-op
	}
	for _, c := range legal {
		if !canTransition(c.from, c.to) {
			t.Errorf("期望合法轉移 %s → %s", c.from, c.to)
		}
		if err := checkTransition(c.from, c.to); err != nil {
			t.Errorf("checkTransition(%s→%s) = %v, 期望 nil", c.from, c.to, err)
		}
	}
}

func TestStateMachine_IllegalTransitions(t *testing.T) {
	illegal := []struct{ from, to protocol.InstanceState }{
		{protocol.InstanceStateStopped, protocol.InstanceStateRunning}, // 必經 Starting
		{protocol.InstanceStateCreated, protocol.InstanceStateRunning},
		{protocol.InstanceStateStopped, protocol.InstanceStateCrashed},
		{protocol.InstanceStateRunning, protocol.InstanceStateCreated},
		{protocol.InstanceStateStopping, protocol.InstanceStateRunning},
	}
	for _, c := range illegal {
		if canTransition(c.from, c.to) {
			t.Errorf("期望非法轉移 %s → %s", c.from, c.to)
		}
		err := checkTransition(c.from, c.to)
		if !errors.Is(err, ErrIllegalTransition) {
			t.Errorf("checkTransition(%s→%s) = %v, 期望 ErrIllegalTransition", c.from, c.to, err)
		}
	}
}
