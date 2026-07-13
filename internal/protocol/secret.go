package protocol

// secretRedacted 是 SecretRef 對外一律回顯的遮罩值。
const secretRedacted = "***"

// SecretRef 是敏感值(RCON 密碼、Palworld AdminPassword、CF_API_KEY 等)的參照(R12)。
//
// 值本身**不落結構**:實際祕密寫入 OS 金鑰庫,SecretRef 僅保存金鑰庫中的鍵名 Key。
// 為確保「不可回顯」,String()/MarshalJSON()/GoString() 一律回傳遮罩值,
// 因此凡經 fmt、log、JSON(DTO/event/log)輸出者皆看不到鍵名或值。
//
// 持久化例外:DB 需保存鍵名以便日後查金鑰庫,由持久化層(T2)直接讀取
// 匯出欄位 Key 存入專屬欄位,而非透過本型別的 JSON 序列化——避免遮罩把鍵名也蓋掉。
// 正因如此本型別刻意不實作 UnmarshalJSON(遮罩後的 "***" 不應被反序列化回鍵名)。
type SecretRef struct {
	// Key 是金鑰庫中的鍵名(如 "RCON_PASSWORD"),對應範本 [[secrets]].key。
	Key string
}

// NewSecretRef 以金鑰庫鍵名建立 SecretRef。
func NewSecretRef(key string) SecretRef {
	return SecretRef{Key: key}
}

// String 回傳遮罩值,使 SecretRef 在 %s/%v 等格式化輸出中不外洩(R12 redaction)。
func (s SecretRef) String() string {
	return secretRedacted
}

// GoString 回傳遮罩後的 Go 語法表示,使 %#v 亦不外洩鍵名。
func (s SecretRef) GoString() string {
	return "protocol.SecretRef{Key:" + `"` + secretRedacted + `"` + "}"
}

// MarshalJSON 一律輸出遮罩字串,確保 SecretRef 出現在任何 DTO/event/log 的
// JSON 中都不會回顯鍵名或值(R12)。
func (s SecretRef) MarshalJSON() ([]byte, error) {
	return []byte(`"` + secretRedacted + `"`), nil
}
