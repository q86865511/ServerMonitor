package agent

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"servermonitor/internal/protocol"
)

// specWithMount 造一個帶 gsm.uuid 標籤與具名 mount 的 spec,供上傳端點測試。
func specWithMount(uuid, mountName, containerPath string) protocol.InstanceSpec {
	s := specWithUUID(uuid)
	s.Mounts = []protocol.MountSpec{{Name: mountName, ContainerPath: containerPath}}
	return s
}

// uploadMountReq 直接以 http 送 PUT 上傳(避免共用 request() 的 JSON 編碼)。
func uploadMountReq(t *testing.T, baseURL, uuid, name, filename, token string, body []byte) int {
	t.Helper()
	url := baseURL + apiBase + "/instances/" + uuid + "/mounts/" + name
	if filename != "" {
		url += "?filename=" + filename
	}
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestUploadMount_Success:上傳到已宣告的 mount → 201,且位元組落入 MockBackend。
func TestUploadMount_Success(t *testing.T) {
	backend := NewMockBackend()
	if _, err := backend.Create(context.Background(), specWithMount("u1", "modpack", "/modpacks")); err != nil {
		t.Fatalf("seed create: %v", err)
	}
	hs := newTestServer(t, backend)

	payload := []byte("PK\x03\x04 fake mrpack bytes")
	status := uploadMountReq(t, hs.URL, "u1", "modpack", "world.mrpack", testToken, payload)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, 期望 201", status)
	}
	got, ok := backend.MountFile("u1", "modpack", "world.mrpack")
	if !ok || !bytes.Equal(got, payload) {
		t.Fatalf("MountFile = %q ok=%v, 期望 %q", got, ok, payload)
	}
}

// TestUploadMount_OverwriteIdempotent:同檔名再上傳 → 覆寫,仍 201。
func TestUploadMount_OverwriteIdempotent(t *testing.T) {
	backend := NewMockBackend()
	_, _ = backend.Create(context.Background(), specWithMount("u1", "modpack", "/modpacks"))
	hs := newTestServer(t, backend)

	if s := uploadMountReq(t, hs.URL, "u1", "modpack", "world.mrpack", testToken, []byte("first")); s != http.StatusCreated {
		t.Fatalf("first upload status = %d", s)
	}
	if s := uploadMountReq(t, hs.URL, "u1", "modpack", "world.mrpack", testToken, []byte("second")); s != http.StatusCreated {
		t.Fatalf("second upload status = %d", s)
	}
	got, _ := backend.MountFile("u1", "modpack", "world.mrpack")
	if string(got) != "second" {
		t.Fatalf("覆寫後內容 = %q, 期望 second", got)
	}
}

// TestUploadMount_Unauthorized:未帶 token → 401,不寫入。
func TestUploadMount_Unauthorized(t *testing.T) {
	backend := NewMockBackend()
	_, _ = backend.Create(context.Background(), specWithMount("u1", "modpack", "/modpacks"))
	hs := newTestServer(t, backend)

	if s := uploadMountReq(t, hs.URL, "u1", "modpack", "world.mrpack", "", []byte("x")); s != http.StatusUnauthorized {
		t.Fatalf("status = %d, 期望 401", s)
	}
	if _, ok := backend.MountFile("u1", "modpack", "world.mrpack"); ok {
		t.Fatal("未授權不應寫入 mount 檔")
	}
}

// TestUploadMount_BadFilename:filename 含路徑穿越/分隔/空 → 400,不寫入。
func TestUploadMount_BadFilename(t *testing.T) {
	backend := NewMockBackend()
	_, _ = backend.Create(context.Background(), specWithMount("u1", "modpack", "/modpacks"))
	hs := newTestServer(t, backend)

	for _, fn := range []string{"..", "a%2Fb", "", "%2e%2e%2fetc"} {
		// a%2Fb / %2e%2e%2f 經 query 解碼後含分隔或 ..;"" 為缺 filename。
		s := uploadMountReq(t, hs.URL, "u1", "modpack", fn, testToken, []byte("x"))
		if s != http.StatusBadRequest {
			t.Fatalf("filename %q status = %d, 期望 400", fn, s)
		}
	}
}

// TestUploadMount_InstanceNotFound:未知實例 → 404。
func TestUploadMount_InstanceNotFound(t *testing.T) {
	backend := NewMockBackend()
	hs := newTestServer(t, backend)
	if s := uploadMountReq(t, hs.URL, "nope", "modpack", "world.mrpack", testToken, []byte("x")); s != http.StatusNotFound {
		t.Fatalf("status = %d, 期望 404", s)
	}
}

// TestUploadMount_MountNotDeclared:實例存在但未宣告該 mount → 404。
func TestUploadMount_MountNotDeclared(t *testing.T) {
	backend := NewMockBackend()
	_, _ = backend.Create(context.Background(), specWithUUID("u1")) // 無 Mounts
	hs := newTestServer(t, backend)
	if s := uploadMountReq(t, hs.URL, "u1", "modpack", "world.mrpack", testToken, []byte("x")); s != http.StatusNotFound {
		t.Fatalf("status = %d, 期望 404", s)
	}
}
