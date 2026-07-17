package provision

import (
	"fmt"
	"os"
	"path/filepath"
)

// eulaContent 是官方 Mojang 伺服器產生的 eula.txt 慣用格式(說明註解行 + eula=true)。
// native 模式下無 itzg 映像代為處理 EULA 環境變數(對照 Docker 模式),故由本套件補上等價行為。
const eulaContent = "#By changing the setting below to TRUE you are indicating your agreement to our EULA (https://aka.ms/MinecraftEULA).\neula=true\n"

// WriteEula 在 dir 下寫入 eula.txt,內容固定為 eula=true(R5:eula 參數為 true 時寫入)。
// dir 不存在時一併建立。
func WriteEula(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("provision: 建立 eula.txt 目的目錄失敗: %w", err)
	}
	dest := filepath.Join(dir, "eula.txt")
	if err := os.WriteFile(dest, []byte(eulaContent), 0o644); err != nil {
		return fmt.Errorf("provision: 寫入 eula.txt 失敗: %w", err)
	}
	return nil
}
