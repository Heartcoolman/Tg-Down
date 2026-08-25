package timeline

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeSidecar 构造一个 sidecar JSON 文件（按下载目录结构：chat_<id>/<type>/<album>/<file>.json）
func writeSidecar(t *testing.T, root string, chatID int64, msgID int64, date time.Time, caption string, albumID int64, mediaType string) string {
	t.Helper()
	dir := filepath.Join(root, "chat_"+itoa(chatID), mediaType)
	if albumID != 0 {
		dir = filepath.Join(root, "chat_"+itoa(chatID), mediaType, "album_"+itoa(albumID))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "f_" + itoa(msgID) + ".jpg"
	chatPos := chatID
	if chatPos < 0 {
		chatPos = -chatPos
	}
	payload := `{"message_id":` + itoa(msgID) + `,"chat_id":` + itoa(chatID) + `,"chat_title":"频道` + itoa(chatID) + `","date":` + itoa(date.Unix()) + `,"date_text":"` + date.Format("2006-01-02 15:04:05") + `","caption":"` + caption + `","album_id":` + itoa(albumID) + `,"media_type":"` + mediaType + `","file_name":"` + name + `","file_size":100,"mime_type":"image/jpeg","unique_id":"u` + itoa(msgID) + `","message_url":"https://t.me/c/` + itoa(chatPos) + `/` + itoa(msgID) + `"}`
	sp := filepath.Join(dir, name+".json")
	if err := os.WriteFile(sp, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	// 媒体文件本体也要存在（扫描器校验 rel path 存在）
	if err := os.WriteFile(filepath.Join(dir, name), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	return sp
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func TestRebuildAndQuery(t *testing.T) {
	root := t.TempDir()
	// 频道 A：两条消息（新、旧）
	writeSidecar(t, root, -1001, 1, time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC), "旧消息", 0, "photo")
	writeSidecar(t, root, -1001, 2, time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC), "新消息", 0, "photo")
	// 频道 B：一条相册消息（album_5）
	writeSidecar(t, root, -1002, 7, time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC), "相册图", 5, "photo")
	// 干扰文件：非 json、系统目录
	_ = os.WriteFile(filepath.Join(root, "chat_-1001", "photo", "noise.txt"), []byte("x"), 0o600)
	_ = os.MkdirAll(filepath.Join(root, ".thumbs"), 0o755)
	_ = os.WriteFile(filepath.Join(root, ".thumbs", "a.jpg.json"), []byte(`{"message_id":99,"chat_id":-1001,"date":1,"media_type":"photo","file_name":"a.jpg"}`), 0o600)
	_ = os.MkdirAll(filepath.Join(root, ".tdlib-files"), 0o755)
	_ = os.WriteFile(filepath.Join(root, ".tdlib-files", "temp", "x.json"), []byte(`{"message_id":98,"chat_id":-1001,"date":1,"media_type":"photo","file_name":"x.jpg"}`), 0o600)

	ix := New()
	if err := ix.Rebuild(root); err != nil {
		t.Fatal(err)
	}

	// 频道列表：2 个频道，按最后同步时间升序（最旧的在前），A 在前
	chats := ix.ChatList()
	if len(chats) != 2 {
		t.Fatalf("频道数 = %d, want 2（.thumbs/.tdlib-files 目录必须被忽略）", len(chats))
	}
	if chats[0].ChatID != -1001 || chats[1].ChatID != -1002 {
		t.Errorf("频道排序 = %v, want A(-1001) 在前", chats)
	}
	if chats[0].Count != 2 {
		t.Errorf("频道 A 摘要 = %+v, want count=2", chats[0])
	}
	if chats[1].Count != 1 || chats[1].LastDateText == "" {
		t.Errorf("频道 B 摘要 = %+v, want count=1 且 last_date_text 非空", chats[1])
	}

	// 全量时间线：3 条，按日期降序
	items, cursor, err := ix.Query(0, 0, 0, 10, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("时间线条目 = %d, want 3", len(items))
	}
	want := []int64{7, 2, 1}
	for i, m := range want {
		if items[i].MessageID != m {
			t.Errorf("第 %d 条 message_id = %d, want %d（应按日期降序）", i, items[i].MessageID, m)
		}
	}
	if cursor != nil {
		t.Errorf("未分页时应无下一页游标, got %+v", cursor)
	}

	// 按频道过滤
	itemsA, _, err := ix.Query(-1001, 0, 0, 10, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(itemsA) != 2 || itemsA[0].MessageID != 2 || itemsA[0].RelPath == "" {
		t.Errorf("频道 A 消息 = %+v, want 2 条且第 1 条 message_id=2、rel_path 非空", itemsA)
	}
	// 相册条目与 caption 透传
	itemB := items[0]
	if itemB.AlbumID != 5 || itemB.Caption != "相册图" || itemB.MessageURL == "" {
		t.Errorf("相册条目字段 = %+v", itemB)
	}
}

func TestQueryPagination(t *testing.T) {
	root := t.TempDir()
	// 频道 A 5 条消息，全部同一天但不同秒（避免同秒排序模糊）：直接用不同 date
	for i := int64(1); i <= 5; i++ {
		writeSidecar(t, root, -1001, i, time.Date(2026, 8, 25, 10, 0, int(i), 0, time.UTC), "m"+itoa(i), 0, "photo")
	}
	ix := New()
	if err := ix.Rebuild(root); err != nil {
		t.Fatal(err)
	}

	// 第 1 页：2 条
	page1, cur, err := ix.Query(-1001, 0, 0, 2, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 2 || page1[0].MessageID != 5 || page1[1].MessageID != 4 {
		t.Fatalf("第 1 页 = %+v", page1)
	}
	if cur == nil {
		t.Fatal("第 1 页后应有游标")
	}

	// 第 2 页：游标接续
	page2, cur2, err := ix.Query(-1001, cur.Date, cur.MessageID, 2, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 2 || page2[0].MessageID != 3 || page2[1].MessageID != 2 {
		t.Fatalf("第 2 页 = %+v", page2)
	}

	// 第 3 页：到最后，无游标
	page3, cur3, err := ix.Query(-1001, cur2.Date, cur2.MessageID, 2, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(page3) != 1 || page3[0].MessageID != 1 {
		t.Fatalf("第 3 页 = %+v", page3)
	}
	if cur3 != nil {
		t.Errorf("最后一页游标应为空, got %+v", cur3)
	}
}

func TestResolvePathRejectsEscape(t *testing.T) {
	root := t.TempDir()
	good := filepath.Join(root, "chat_-1001", "photo", "a.jpg")
	if err := os.MkdirAll(filepath.Dir(good), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(good, []byte("x"), 0o600)

	// 合法相对路径
	if got, err := ResolvePath(root, "chat_-1001/photo/a.jpg"); err != nil || got != good {
		t.Errorf("ResolvePath 合法路径 = %q, %v", got, err)
	}
	// 各种逃逸尝试必须拒绝
	for _, evil := range []string{
		"../etc/passwd",
		"chat_-1001/../../etc/passwd",
		"/etc/passwd",
		"..",
		"chat_-1001/photo/../../x",
		"",
	} {
		if _, err := ResolvePath(root, evil); err == nil {
			t.Errorf("逃逸路径 %q 未被拒绝", evil)
		}
	}
	// 不存在的文件拒绝
	if _, err := ResolvePath(root, "chat_-1001/photo/nope.jpg"); err == nil {
		t.Error("不存在文件未被拒绝")
	}
}

func TestCorruptSidecarSkipped(t *testing.T) {
	root := t.TempDir()
	writeSidecar(t, root, -1001, 1, time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC), "好", 0, "photo")
	bad := filepath.Join(root, "chat_-1001", "photo", "broken.jpg.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(root, "chat_-1001", "photo", "broken.jpg"), []byte("x"), 0o600)

	ix := New()
	if err := ix.Rebuild(root); err != nil {
		t.Fatal(err)
	}
	items, _, err := ix.Query(-1001, 0, 0, 10, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].MessageID != 1 {
		t.Errorf("损坏 sidecar 未被跳过: %+v", items)
	}
}

// TestQueryTagFilter 覆盖标签过滤：词边界匹配、大小写不敏感、跨页不丢
func TestQueryTagFilter(t *testing.T) {
	root := t.TempDir()
	// 频道 A 4 条消息，按时间排列：最新在最前
	// msg1: 无标签；msg2: #马赛克；msg3: #马赛克 且 "#马赛克X" 前缀干扰；msg4: #马赛克
	writeSidecar(t, root, -1001, 1, time.Date(2026, 8, 25, 10, 0, 4, 0, time.UTC), "没有标签", 0, "photo")
	writeSidecar(t, root, -1001, 2, time.Date(2026, 8, 25, 10, 0, 3, 0, time.UTC), "带#马赛克 标签", 0, "photo")
	writeSidecar(t, root, -1001, 3, time.Date(2026, 8, 25, 10, 0, 2, 0, time.UTC), "前缀#马赛克X不算 顺带#mosaic", 0, "photo")
	writeSidecar(t, root, -1001, 4, time.Date(2026, 8, 25, 10, 0, 1, 0, time.UTC), "结尾#马赛克", 0, "photo")

	ix := New()
	if err := ix.Rebuild(root); err != nil {
		t.Fatal(err)
	}

	// 大小写不敏感 + 词边界（#马赛克X 不算，#mosaic 算）
	hits, _, err := ix.Query(-1001, 0, 0, 10, []string{"马赛克"}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{2, 4}
	if len(hits) != len(want) {
		t.Fatalf("标签命中 = %v, want message 2/4（#马赛克X 不应命中）", idsOf(hits))
	}
	for i, m := range want {
		if hits[i].MessageID != m {
			t.Errorf("第 %d 条 = msg %d, want %d", i, hits[i].MessageID, m)
		}
	}

	// 分页 + 标签：页大小 1，应逐条返回 2 → 4，不丢
	var got []int64
	var cursor *PageCursor
	for page := 0; page < 5; page++ {
		var items []Entry
		if cursor == nil {
			items, cursor, err = ix.Query(-1001, 0, 0, 1, []string{"马赛克"}, false)
		} else {
			items, cursor, err = ix.Query(-1001, cursor.Date, cursor.MessageID, 1, []string{"马赛克"}, false)
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			got = append(got, it.MessageID)
		}
		if cursor == nil {
			break
		}
	}
	if len(got) != 2 || got[0] != 2 || got[1] != 4 {
		t.Errorf("标签分页结果 = %v, want [2 4]", got)
	}
}

func idsOf(list []Entry) []int64 {
	out := make([]int64, 0, len(list))
	for _, e := range list {
		out = append(out, e.MessageID)
	}
	return out
}

func TestTagMulti(t *testing.T) {
	root := t.TempDir()
	writeSidecar(t, root, -1001, 1, time.Date(2026, 8, 25, 10, 0, 4, 0, time.UTC), "无标签", 0, "photo")
	writeSidecar(t, root, -1001, 2, time.Date(2026, 8, 25, 10, 0, 3, 0, time.UTC), "带 #alpha", 0, "photo")
	writeSidecar(t, root, -1001, 3, time.Date(2026, 8, 25, 10, 0, 2, 0, time.UTC), "双 #alpha #beta", 0, "photo")
	writeSidecar(t, root, -1001, 4, time.Date(2026, 8, 25, 10, 0, 1, 0, time.UTC), "带 #beta", 0, "photo")

	ix := New()
	if err := ix.Rebuild(root); err != nil {
		t.Fatal(err)
	}

	// 并集：alpha 或 beta → 2,3,4
	hits, _, err := ix.Query(-1001, 0, 0, 10, []string{"alpha", "beta"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := idsOf(hits); len(got) != 3 || got[0] != 2 || got[1] != 3 || got[2] != 4 {
		t.Errorf("并集结果 = %v, want [2 3 4]", got)
	}

	// 交集：同时含 alpha 和 beta → 只有 3
	hits, _, err = ix.Query(-1001, 0, 0, 10, []string{"alpha", "beta"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := idsOf(hits); len(got) != 1 || got[0] != 3 {
		t.Errorf("交集结果 = %v, want [3]", got)
	}

	// 交集：含一个不存在标签 → 空
	hits, _, err = ix.Query(-1001, 0, 0, 10, []string{"alpha", "nope"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := idsOf(hits); len(got) != 0 {
		t.Errorf("不存在标签交集 = %v, want []", got)
	}

	// 空标签 = 不过滤
	hits, _, err = ix.Query(-1001, 0, 0, 10, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := idsOf(hits); len(got) != 4 {
		t.Errorf("无标签结果 = %v, want 4 条", got)
	}
}