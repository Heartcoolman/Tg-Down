package downloader

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	mediapkg "tg-down/internal/media"
)

// TestDefaultTemplate_ReproducesLegacyLayout 是 v3.0 最不能失手的一条约束：
// 默认模板必须逐字节复刻 v2.x 的硬编码布局。它一旦漂移，所有既有用户的文件都会被认作
// "不存在"而重新下载一遍——v2.0 引入相册目录时已经踩过这个坑。
func TestDefaultTemplate_ReproducesLegacyLayout(t *testing.T) {
	root := t.TempDir()

	tests := []struct {
		name           string
		media          *MediaInfo
		classifyByType bool
		want           string // 相对于下载根目录
	}{
		{
			name:           "分类存储 + 非相册",
			media:          &MediaInfo{ChatID: -100123, MediaType: mediapkg.Photo, FileName: "photo_-100123_42.jpg"},
			classifyByType: true,
			want:           "chat_-100123/photo/photo_-100123_42.jpg",
		},
		{
			name:           "分类存储 + 相册",
			media:          &MediaInfo{ChatID: -100123, MediaType: mediapkg.Video, FileName: "42_v.mp4", AlbumID: 777},
			classifyByType: true,
			want:           "chat_-100123/video/album_777/42_v.mp4",
		},
		{
			name:           "关闭分类存储时 type 层级消失",
			media:          &MediaInfo{ChatID: -100123, MediaType: mediapkg.Photo, FileName: "p.jpg"},
			classifyByType: false,
			want:           "chat_-100123/p.jpg",
		},
		{
			name:           "关闭分类存储 + 相册",
			media:          &MediaInfo{ChatID: 55, MediaType: mediapkg.Photo, FileName: "p.jpg", AlbumID: 9},
			classifyByType: false,
			want:           "chat_55/album_9/p.jpg",
		},
		{
			name:           "缺失文件名时按消息 id 合成",
			media:          &MediaInfo{ChatID: 55, MediaType: mediapkg.Video, MessageID: 42, TDFileID: 7, MimeType: "video/mp4"},
			classifyByType: true,
			want:           "chat_55/video/file_42_7.mp4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := New(root, 1, nil)
			d.SetClassifyByType(tt.classifyByType)

			_, _, got := d.planMediaPath(tt.media)
			want := filepath.Join(root, filepath.FromSlash(tt.want))
			if got != want {
				t.Errorf("落盘路径 = %q\n期望         = %q", got, want)
			}
		})
	}
}

// TestPlanMediaPath_CustomTemplate 覆盖自定义模板的展开
func TestPlanMediaPath_CustomTemplate(t *testing.T) {
	root := t.TempDir()
	media := &MediaInfo{
		ChatID: -100123, ChatTitle: "我的频道", MediaType: mediapkg.Video,
		FileName: "42_v.mp4", MessageID: 42, SenderID: 888,
		Date: time.Date(2024, 3, 5, 10, 0, 0, 0, time.UTC),
	}

	tests := []struct {
		name string
		tpl  string
		want string
	}{
		{"标题目录", "{chat_id}/{chat_title}/{name}", "-100123/我的频道/42_v.mp4"},
		{"按日期归档", "{chat_id}/{chat_title}/{date}/{name}", "-100123/我的频道/2024-03-05/42_v.mp4"},
		{"按发送者归档", "{chat_id}/{sender}/{name}", "-100123/888/42_v.mp4"},
		{"消息 id + 扩展名", "{chat_id}/{msg_id}{ext}", "-100123/42.mp4"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if problem := ValidatePathTemplate(tt.tpl); problem != "" {
				t.Fatalf("模板 %q 被判为非法: %s", tt.tpl, problem)
			}
			d := New(root, 1, nil)
			d.SetPathTemplate(tt.tpl)

			_, _, got := d.planMediaPath(media)
			want := filepath.Join(root, filepath.FromSlash(tt.want))
			if got != want {
				t.Errorf("落盘路径 = %q\n期望         = %q", got, want)
			}
		})
	}
}

// TestPlanMediaPath_ChatTitleFallsBackToID 校验标题缺失时不会塌掉一层目录：
// {chat_title} 若展开成空串，该路径段被丢弃，不同聊天的文件就会混进同一个目录。
func TestPlanMediaPath_ChatTitleFallsBackToID(t *testing.T) {
	root := t.TempDir()
	d := New(root, 1, nil)
	d.SetPathTemplate("{chat_id}/{chat_title}/{name}")

	_, _, got := d.planMediaPath(&MediaInfo{ChatID: -100123, FileName: "a.jpg"})
	want := filepath.Join(root, "-100123", "chat_-100123", "a.jpg")
	if got != want {
		t.Errorf("落盘路径 = %q, want %q", got, want)
	}
}

// TestPlanMediaPath_ChatTitleAsTopDir 校验纯标题模板（不含 {chat_id}）：
// 顶层目录即群聊名；标题缺失时回退 chat_<id>，聊天隔离不塌。
func TestPlanMediaPath_ChatTitleAsTopDir(t *testing.T) {
	root := t.TempDir()
	d := New(root, 1, nil)
	d.SetClassifyByType(true)
	d.SetPathTemplate("{chat_title}/{type}/{album}/{name}")

	// 有标题：顶层目录 = 群聊名
	_, _, got := d.planMediaPath(&MediaInfo{
		ChatID: -100123, ChatTitle: "我的频道", MediaType: mediapkg.Photo,
		FileName: "photo_-100123_42.jpg",
	})
	want := filepath.Join(root, "我的频道", "photo", "photo_-100123_42.jpg")
	if got != want {
		t.Errorf("落盘路径 = %q\n期望         = %q", got, want)
	}

	// 标题缺失：回退 chat_<id>，不同聊天的文件仍互不混淆
	d2 := New(root, 1, nil)
	d2.SetClassifyByType(true)
	d2.SetPathTemplate("{chat_title}/{type}/{name}")

	_, _, gotNoTitle := d2.planMediaPath(&MediaInfo{
		ChatID: -100123, MediaType: mediapkg.Photo, FileName: "p.jpg", MessageID: 9,
	})
	wantNoTitle := filepath.Join(root, "chat_-100123", "photo", "p.jpg")
	if gotNoTitle != wantNoTitle {
		t.Errorf("空标题回退路径 = %q\n期望           = %q", gotNoTitle, wantNoTitle)
	}

	// 两个不同聊天即使都没有标题，也落到各自目录
	d3 := New(root, 1, nil)
	d3.SetClassifyByType(true)
	d3.SetPathTemplate("{chat_title}/{type}/{name}")

	_, _, gotA := d3.planMediaPath(&MediaInfo{ChatID: 1, MediaType: mediapkg.Photo, FileName: "a.jpg", MessageID: 1})
	_, _, gotB := d3.planMediaPath(&MediaInfo{ChatID: 2, MediaType: mediapkg.Photo, FileName: "a.jpg", MessageID: 1})
	if gotA == gotB {
		t.Fatalf("不同聊天生成了相同路径: %s", gotA)
	}
}

// TestPlanMediaPath_MaliciousNamesStayInRoot 校验来自 Telegram 的文件名/标题
// （他人可控）无法越出下载根目录
func TestPlanMediaPath_MaliciousNamesStayInRoot(t *testing.T) {
	root := t.TempDir()
	d := New(root, 1, nil)
	d.SetPathTemplate("{chat_id}/{chat_title}/{name}")

	for _, tc := range []struct{ title, name string }{
		{"../../etc", "passwd"},
		{"ok", "../../../../etc/passwd"},
		{"a/b", "c/d.jpg"},
		{"..", ".."},
	} {
		_, _, got := d.planMediaPath(&MediaInfo{ChatID: 1, ChatTitle: tc.title, FileName: tc.name, MessageID: 7})
		if !strings.HasPrefix(got, root+string(filepath.Separator)) {
			t.Errorf("标题 %q / 文件名 %q 逃出了下载根目录: %s", tc.title, tc.name, got)
		}
	}
}

// TestSetPathTemplate_RejectsInvalid 校验非法模板被忽略并退回默认布局，
// 而不是让配置里的一个手滑把文件写到奇怪的地方
func TestSetPathTemplate_RejectsInvalid(t *testing.T) {
	root := t.TempDir()
	media := &MediaInfo{ChatID: 1, MediaType: mediapkg.Photo, FileName: "a.jpg", MessageID: 7}

	for _, tpl := range []string{
		"",                     // 空
		"/abs/{name}",          // 绝对路径
		"../{name}",            // 越界
		"{chat_id}/{unknown}",  // 未知占位符
		"{chat_id}/{chat_id}",  // 无唯一性占位符 → 同名文件互相覆盖
		"{chat_title}/{album}", // 同上
	} {
		d := New(root, 1, nil)
		d.SetClassifyByType(true)
		d.SetPathTemplate(tpl)

		_, _, got := d.planMediaPath(media)
		want := filepath.Join(root, "chat_1", "photo", "a.jpg")
		if got != want {
			t.Errorf("非法模板 %q 未退回默认布局: got %q, want %q", tpl, got, want)
		}
	}
}

func TestValidatePathTemplate(t *testing.T) {
	valid := []string{
		DefaultPathTemplate,
		"{chat_id}/{name}",
		"{chat_id}/{chat_title}/{date}/{name}",
		"{chat_id}/{msg_id}{ext}",
		// 无 {chat_id} 的纯标题隔离模板：标题缺失时回退 chat_<id>，聊天隔离不塌
		"{chat_title}/{type}/{album}/{name}",
		"{chat_title}/{date}/{name}",
	}
	for _, tpl := range valid {
		if problem := ValidatePathTemplate(tpl); problem != "" {
			t.Errorf("模板 %q 应合法，却报: %s", tpl, problem)
		}
	}

	invalid := []string{
		"", "  ", "/{name}", "a/../{name}", "{name}\\{ext}", "{nope}/{name}",
		"{chat_id}/{type}", "{name}", "{sender}/{date}/{name}",
		"{chat_id}/{ChatID}/{name}", "{chat_id}/{name-typo}", "{chat_id}/{name", "{chat_id}/name}",
	}
	for _, tpl := range invalid {
		if ValidatePathTemplate(tpl) == "" {
			t.Errorf("模板 %q 应被判非法，却通过了", tpl)
		}
	}
}

func TestPlanMediaPath_CustomTemplateSeparatesChats(t *testing.T) {
	d := New(t.TempDir(), 1, nil)
	d.SetPathTemplate("{chat_id}/{name}")

	first := d.TargetPath(&MediaInfo{ChatID: 1, MessageID: 9, FileName: "same.jpg"})
	second := d.TargetPath(&MediaInfo{ChatID: 2, MessageID: 9, FileName: "same.jpg"})
	if first == second {
		t.Fatalf("不同聊天生成了相同路径: %s", first)
	}
}

// TestSanitizeSegment 覆盖 Windows 专有的三类坑（M5 依赖）
func TestSanitizeSegment(t *testing.T) {
	tests := []struct{ in, want string }{
		{"normal.jpg", "normal.jpg"},
		{"a/b\\c:d*e?f\"g<h>i|j", "a_b_c_d_e_f_g_h_i_j"},
		{"..", "_"},
		{"", ""},
		{".", ""},
		// Windows 保留设备名：带扩展名也一样创建不出来
		{"CON", "_CON"},
		{"nul.txt", "_nul.txt"},
		{"com1.jpg", "_com1.jpg"},
		{"console.txt", "console.txt"}, // 只有精确匹配才是保留名
		// Windows 会静默吃掉结尾的点与空格
		{"trailing.", "trailing"},
		{"trailing ", "trailing"},
		{"trailing. . ", "trailing"},
	}
	for _, tt := range tests {
		if got := sanitizeSegment(tt.in); got != tt.want {
			t.Errorf("sanitizeSegment(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestSanitizeSegment_Truncation 校验超长文件名被截断、扩展名保留、且不切碎 UTF-8
func TestSanitizeSegment_Truncation(t *testing.T) {
	long := strings.Repeat("a", 500) + ".jpg"
	got := sanitizeSegment(long)
	if len(got) > maxFileNameLen {
		t.Errorf("截断后长度 = %d，超过上限 %d", len(got), maxFileNameLen)
	}
	if !strings.HasSuffix(got, ".jpg") {
		t.Errorf("截断后应保留扩展名，得到 %q", got)
	}

	// 中文名：截断点不得落在多字节字符中间
	cn := strings.Repeat("中", 200) + ".mp4"
	gotCN := sanitizeSegment(cn)
	if len(gotCN) > maxFileNameLen {
		t.Errorf("截断后长度 = %d，超过上限 %d", len(gotCN), maxFileNameLen)
	}
	if !utf8Valid(gotCN) {
		t.Errorf("截断产生了非法 UTF-8: %q", gotCN)
	}
	if !strings.HasSuffix(gotCN, ".mp4") {
		t.Errorf("截断后应保留扩展名，得到 %q", gotCN)
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}
