package core

import (
	"errors"
	"fmt"

	"servermonitor/internal/protocol"
)

// ErrIllegalTransition 表示嘗試了狀態機不允許的轉移(R8)。攜帶 from/to 供診斷。
// 對帳(Reconciler)以實際為準的 observed 修正屬「權威覆寫」,不走本轉移守衛,故不受此限。
var ErrIllegalTransition = errors.New("core: 非法狀態轉移")

// allowedTransitions 是每實例狀態機的合法轉移表(R8;desired ∈ {Running, Stopped})。
// 以 (from → 可達 to 集合)表達 design「狀態機與對帳」段的轉移表:轉移是否合法只看邊是否存在,
// 由編排層(Orchestrator)決定目標狀態、由本表驗證其合法性;非法轉移被拒(見 checkTransition)。
//
// 幾個刻意的設計:
//   - 同狀態→同狀態視為 no-op(見 checkTransition),不列於此表。
//   - BackingUp/Restoring 的「回到原狀態」以「可達 Running 或 Stopped」表達(T9 依原 desired 決定其一)。
//   - Offline 為節點層;由對帳權威修正收斂,故其出邊涵蓋各具體狀態(供恢復後回填)。
//   - Crashed 的重試/放棄(→Starting / →Error)與自動重啟屬 T11;本表先備妥邊,T8 不驅動之。
var allowedTransitions = map[protocol.InstanceState]map[protocol.InstanceState]bool{
	protocol.InstanceStateCreated: {
		protocol.InstanceStateStarting: true,
		protocol.InstanceStateOffline:  true,
		protocol.InstanceStateError:    true,
	},
	protocol.InstanceStateStarting: {
		protocol.InstanceStateRunning:  true, // 就緒
		protocol.InstanceStateError:    true, // 就緒逾時
		protocol.InstanceStateCrashed:  true, // 啟動途中 die
		protocol.InstanceStateStopping: true, // 啟動途中被要求停止
		protocol.InstanceStateOffline:  true,
	},
	protocol.InstanceStateRunning: {
		protocol.InstanceStateStopping:  true, // planned stop
		protocol.InstanceStateCrashed:   true, // 非計畫 die / 探針卡死(探針 T11)
		protocol.InstanceStateBackingUp: true,
		protocol.InstanceStateRestoring: true,
		protocol.InstanceStateOffline:   true,
	},
	protocol.InstanceStateStopping: {
		protocol.InstanceStateStopped: true,
		protocol.InstanceStateError:   true, // 停止失敗
		protocol.InstanceStateOffline: true,
	},
	protocol.InstanceStateStopped: {
		protocol.InstanceStateStarting:  true,
		protocol.InstanceStateBackingUp: true,
		protocol.InstanceStateRestoring: true,
		protocol.InstanceStateOffline:   true,
	},
	protocol.InstanceStateCrashed: {
		protocol.InstanceStateStarting: true, // 重試(T11)或手動重啟
		protocol.InstanceStateError:    true, // 達上限 RESTART_GIVEUP(T11)
		protocol.InstanceStateStopping: true, // 使用者停止已崩潰實例
		protocol.InstanceStateStopped:  true,
		protocol.InstanceStateOffline:  true,
	},
	protocol.InstanceStateBackingUp: {
		protocol.InstanceStateRunning: true,
		protocol.InstanceStateStopped: true,
		protocol.InstanceStateError:   true,
		protocol.InstanceStateOffline: true,
	},
	protocol.InstanceStateRestoring: {
		protocol.InstanceStateRunning:  true,
		protocol.InstanceStateStopped:  true,
		protocol.InstanceStateStarting: true,
		protocol.InstanceStateError:    true,
		protocol.InstanceStateOffline:  true,
	},
	protocol.InstanceStateError: {
		protocol.InstanceStateStarting: true, // 手動恢復
		protocol.InstanceStateStopping: true,
		protocol.InstanceStateStopped:  true,
		protocol.InstanceStateOffline:  true,
	},
	protocol.InstanceStateOffline: {
		// 對帳後收斂:回填至實際狀態(以下為權威修正時的允許目標)。
		protocol.InstanceStateRunning:  true,
		protocol.InstanceStateStopped:  true,
		protocol.InstanceStateCrashed:  true,
		protocol.InstanceStateCreated:  true,
		protocol.InstanceStateStarting: true,
		protocol.InstanceStateError:    true,
	},
}

// canTransition 回報 from→to 是否為合法轉移(同狀態視為合法的 no-op)。
func canTransition(from, to protocol.InstanceState) bool {
	if from == to {
		return true
	}
	return allowedTransitions[from][to]
}

// checkTransition 驗證 from→to;非法時回 ErrIllegalTransition(含 from/to)。
func checkTransition(from, to protocol.InstanceState) error {
	if canTransition(from, to) {
		return nil
	}
	return fmt.Errorf("%w: %s → %s", ErrIllegalTransition, from, to)
}
