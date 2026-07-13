// Package protocol 定義跨層共享的版本化型別:遊戲範本 schema(R1)、
// 執行後端與代理 API 的 DTO(R4/R5)、結構化事件封套與碼表(R14)、
// 以及敏感值參照 SecretRef(R12)。
//
// 本套件只含資料型別與最小解析輔助,不含任何 core/agent 的行為邏輯——
// 範本驗證、狀態機轉移、持久化等分別由 internal/core 與 internal/agent 實作。
// 對應設計:specs/game-server-manager/design.md「介面與資料模型」。
package protocol
