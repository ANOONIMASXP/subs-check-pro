package substore

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWritePassthroughOfficialBytes 核心断言：Go 层不得对官方序列化结果做二次 Marshal，
// 否则键序会被重排成字母序，与官方 JSON.stringify(cache, null, 2) 不一致。
func TestWritePassthroughOfficialBytes(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "sub-store.json")
	store, err := NewLoonKVStore(mainPath)
	if err != nil {
		t.Fatalf("初始化存储失败: %v", err)
	}

	// 官方产物：2 空格缩进 + 插入顺序（刻意非字母序）
	official := "{\n  \"schemaVersion\": \"2.0\",\n  \"subs\": [],\n  \"settings\": {}\n}"
	value := official
	if !store.Write(&value, "sub-store") {
		t.Fatal("写入失败")
	}

	got, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatalf("读取文件失败: %v", err)
	}
	if string(got) != official {
		t.Errorf("字节被改写:\n got: %q\nwant: %q", string(got), official)
	}

	if read := store.Read("sub-store"); read != official {
		t.Errorf("Read 返回值与文件不一致: %q", read)
	}

	// 非法 JSON 不得覆盖磁盘上的合法文件
	bad := "{ not json"
	if store.Write(&bad, "sub-store") {
		t.Error("非法 JSON 应当被拒绝")
	}
	got, _ = os.ReadFile(mainPath)
	if string(got) != official {
		t.Errorf("非法写入覆盖了合法文件: %q", string(got))
	}

	// value == nil → 删除文件
	store.Write(nil, "sub-store")
	if _, err := os.Stat(mainPath); !os.IsNotExist(err) {
		t.Error("value 为 nil 时应当删除文件")
	}
}
