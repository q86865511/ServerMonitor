package protocol

import (
	"encoding/json"
	"time"
)

// Severity 是事件嚴重度。
type Severity string

const (
	SeverityInfo    Severity = "info"
	SeverityWarning Severity = "warning"
	SeverityError   Severity = "error"
)

// EventCode 是結構化事件的代碼(R14 必備碼表,見下方常數)。
type EventCode string

// 必備 event code 目錄(對齊 design「事件封套與碼表」)。
// 其他需求(R1/R2/R8/R9/R10/R12/R13)的「已記錄」驗收以對應 code + 欄位判定。
const (
	EventTemplateLoadFailed   EventCode = "TEMPLATE_LOAD_FAILED"
	EventInstanceCreated      EventCode = "INSTANCE_CREATED"
	EventInstanceCreateFailed EventCode = "INSTANCE_CREATE_FAILED"
	EventInstanceStarted      EventCode = "INSTANCE_STARTED"
	EventInstanceStopped      EventCode = "INSTANCE_STOPPED"
	EventInstanceCrashed      EventCode = "INSTANCE_CRASHED"
	EventInstanceRestarted    EventCode = "INSTANCE_RESTARTED"
	EventRestartGiveup        EventCode = "RESTART_GIVEUP"
	EventHealthProbeFailed    EventCode = "HEALTH_PROBE_FAILED"
	EventBackupStarted        EventCode = "BACKUP_STARTED"
	EventBackupCompleted      EventCode = "BACKUP_COMPLETED"
	EventBackupFailed         EventCode = "BACKUP_FAILED"
	EventRestoreStarted       EventCode = "RESTORE_STARTED"
	EventRestoreCompleted     EventCode = "RESTORE_COMPLETED"
	EventRestoreFailed        EventCode = "RESTORE_FAILED"
	EventAlertSent            EventCode = "ALERT_SENT"
	EventAlertFailed          EventCode = "ALERT_FAILED"
	EventReconcileOrphan      EventCode = "RECONCILE_ORPHAN"
	EventReconcileMismatch    EventCode = "RECONCILE_MISMATCH"
	EventNodeOffline          EventCode = "NODE_OFFLINE"
	EventDBQuarantine         EventCode = "DB_QUARANTINE"
)

// RequiredEventCodes 列出 design 明訂的必備 event code,供 EventLog(T2)自驗碼表齊全。
var RequiredEventCodes = []EventCode{
	EventTemplateLoadFailed,
	EventInstanceCreated,
	EventInstanceCreateFailed,
	EventInstanceStarted,
	EventInstanceStopped,
	EventInstanceCrashed,
	EventInstanceRestarted,
	EventRestartGiveup,
	EventHealthProbeFailed,
	EventBackupStarted,
	EventBackupCompleted,
	EventBackupFailed,
	EventRestoreStarted,
	EventRestoreCompleted,
	EventRestoreFailed,
	EventAlertSent,
	EventAlertFailed,
	EventReconcileOrphan,
	EventReconcileMismatch,
	EventNodeOffline,
	EventDBQuarantine,
}

// Event 是統一的結構化事件封套(R14)。可空欄位以指標表示,對應 DB events 表欄位。
// DetailsJSON 承載各 code 專屬的細節(如崩潰退出碼、對帳差異),以原始 JSON 保存。
type Event struct {
	Code         EventCode       `json:"code"`
	TsUTC        time.Time       `json:"ts_utc"`
	Severity     Severity        `json:"severity"`
	InstanceUUID *string         `json:"instance_uuid,omitempty"`
	Node         *string         `json:"node,omitempty"`
	TemplateID   *string         `json:"template_id,omitempty"`
	DetailsJSON  json.RawMessage `json:"details_json,omitempty"`
}
