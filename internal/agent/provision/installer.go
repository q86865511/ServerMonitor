package provision

import "context"

// InstallRequest 是一次伺服器安裝請求的輸入。
type InstallRequest struct {
	// MCVersion 是要安裝的 Minecraft 版本號(如 "1.20.4")。
	MCVersion string
	// TargetDir 是安裝目的目錄(通常為實例工作目錄);Vanilla/Paper 會在此落地 server.jar。
	TargetDir string
	// JavaExe 是供 installer 類 loader(Fabric/Forge/NeoForge,後續任務 T4)執行官方
	// installer jar 所需的 java 執行檔路徑;本任務的 Vanilla/Paper 兩實作純下載不執行 Java,
	// 用不到此欄位,先留供後續任務使用。
	JavaExe string
}

// InstalledServer 描述安裝完成後啟動所需的產物路徑。
type InstalledServer struct {
	// ServerJar 是可直接以 `java -jar` 啟動的伺服器 jar 絕對路徑;Vanilla/Paper 填此欄位。
	ServerJar string
	// StartScript 與 ArgsFile 留給 Forge/NeoForge 類 loader(後續任務 T4)——依 design.md
	// 的風險緩解,以官方 installer 產出的啟動腳本/args 檔為啟動來源、不自解析 installer 輸出
	// 結構;本任務的 Vanilla/Paper 不產生這兩者。
	StartScript string
	ArgsFile    string
}

// ServerInstaller 依 variant 將 Minecraft 伺服器檔案安裝至 InstallRequest.TargetDir(R5)。
// 各實作純負責取得位元組並安全落地,不碰 Job Object / PID(supervisor 職責)。
type ServerInstaller interface {
	Install(ctx context.Context, req InstallRequest, progress ProgressFunc) (InstalledServer, error)
}
