package core

import (
	"context"

	"servermonitor/internal/protocol"
)

// AlertSink 是「把一則事件送往外部告警管道」的抽象埠(R8/R10)。RestartPolicy 於崩潰迴圈
// 達上限、Scheduler 於排程備份失敗時經此埠發出告警;Discord webhook 等具體通道屬 T13,於此
// 埠實作(含窗口/hysteresis/cooldown/dedup)。core 端只依賴本介面,不硬編通道。
//
// 契約:Alert 應「快速返回」——它可能在持有 per-instance lock 的路徑上被呼叫(見
// RestartPolicy),故實作若需做阻塞式網路 I/O(如 HTTP webhook),應自行非同步化,不得於
// 呼叫執行緒阻塞。RestartPolicy 本身已把 Alert 分派到 lock 之外的執行緒(見 dispatchAlert),
// 但介面契約仍要求實作端不假設呼叫執行緒可長時間佔用。
type AlertSink interface {
	// Alert 送出一則告警事件;送達失敗回錯誤(由呼叫端記 ALERT_FAILED)。
	Alert(ctx context.Context, ev protocol.Event) error
}

// NopAlertSink 是 AlertSink 的 no-op 預設(不送任何告警、恆成功),供未配置通道時使用。
type NopAlertSink struct{}

// Alert 實作 AlertSink,為 no-op。
func (NopAlertSink) Alert(context.Context, protocol.Event) error { return nil }

var _ AlertSink = NopAlertSink{}
