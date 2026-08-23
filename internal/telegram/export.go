package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
	"time"

	tdclient "github.com/zelenin/go-tdlib/client"
)

const (
	// exportDirName 是导出产物在下载根目录下的子目录
	exportDirName = "exports"
	// exportFilePerm/exportDirPerm 是导出文件与目录的权限。导出中含完整聊天正文，
	// 文件只允许当前用户读取。
	exportFilePerm = 0o600
	exportDirPerm  = 0o750
	// exportLogEvery 控制导出进度日志的频率（消息条数）
	exportLogEvery = 5000
)

// 媒体扩展名常量（贴纸格式与导出渲染共用）
const (
	extWebp = ".webp"
	extWebm = ".webm"
	extTgs  = ".tgs"
	extMp4  = ".mp4"
)

// exportMessage 是导出的单条消息
type exportMessage struct {
	ID       int64  `json:"id"`
	Date     int64  `json:"date"`
	SenderID int64  `json:"sender_id"`
	Text     string `json:"text,omitempty"`
	// MediaType/FileName 描述消息附带的媒体（无媒体时为空）
	MediaType string `json:"media_type,omitempty"`
	FileName  string `json:"file_name,omitempty"`
	FileSize  int64  `json:"file_size,omitempty"`
	// FilePath 是相对导出文件的媒体路径；媒体尚未下载时为空
	FilePath string `json:"file_path,omitempty"`
	AlbumID  int64  `json:"album_id,omitempty"`
}

// exportPayload 是 JSON 导出的顶层结构
type exportPayload struct {
	ChatID     int64           `json:"chat_id"`
	ChatTitle  string          `json:"chat_title,omitempty"`
	ExportedAt int64           `json:"exported_at"`
	Count      int             `json:"count"`
	Truncated  bool            `json:"truncated"`
	Messages   []exportMessage `json:"messages"`
}

// ExportChat 把一个聊天导出为 JSON 与自包含 HTML。
//
// 与下载扫描的关键差异：导出要的是**全部消息**（含纯文本），因此使用
// SearchMessagesFilterEmpty 枚举完整历史，并以 NextFromMessageId == 0 作为明确结束信号。
//
// 媒体不重新下载：按当前路径模板算出它应在的位置，文件真在那儿才写进导出（相对路径），
// 于是导出的 HTML 可以直接在本地打开、点开图片和视频。
func (c *Client) ExportChat(ctx context.Context, spec ExportSpec) (*ExportResult, error) {
	td := c.client()
	if td == nil {
		return nil, fmt.Errorf("TDLib 未连接")
	}

	closeChat, err := c.openChatForHistory(ctx, td, spec.ChatID)
	if err != nil {
		return nil, err
	}
	if closeChat != nil {
		defer closeChat()
	}

	msgs, truncated, err := c.collectExportMessages(ctx, td, spec)
	if err != nil {
		return nil, err
	}

	return c.writeExport(spec, msgs, truncated)
}

// collectExportMessages 倒序翻完整条历史，收集消息；返回的结果按时间正序（老 → 新），
// 因为人读导出时是从头往后读的
func (c *Client) collectExportMessages(
	ctx context.Context, td tdAPI, spec ExportSpec,
) (msgs []exportMessage, truncated bool, err error) {
	limit := int32(DefaultMessageLimit)
	fromMsgID := int64(0)

	for {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}

		found, err := c.searchChatMessagesRequest(ctx, td, &tdclient.SearchChatMessagesRequest{
			ChatId:        spec.ChatID,
			FromMessageId: fromMsgID,
			Limit:         limit,
			Filter:        &tdclient.SearchMessagesFilterEmpty{},
		})
		if err != nil {
			return nil, false, err
		}
		page := found.Messages

		for _, m := range page {
			msgs = append(msgs, c.toExportMessage(m, spec.ChatTitle))
			if spec.Limit > 0 && len(msgs) >= spec.Limit {
				return reverseMessages(msgs), true, nil
			}
		}
		if len(msgs)%exportLogEvery < len(page) {
			c.logger.Info("导出进度：已收集 %d 条消息", len(msgs))
		}

		next := found.NextFromMessageId
		if next == 0 {
			break
		}
		if next == fromMsgID {
			return nil, false, fmt.Errorf("导出聊天消息游标未推进: %d", next)
		}
		fromMsgID = next
	}
	return reverseMessages(msgs), false, nil
}

// toExportMessage 把一条 TDLib 消息转成导出条目；媒体已在本地时记录其绝对路径
// （writeExport 会再改写成相对导出文件的路径）
func (c *Client) toExportMessage(m *tdclient.Message, chatTitle string) exportMessage {
	em := exportMessage{
		ID:       m.Id,
		Date:     int64(m.Date),
		SenderID: senderID(m.SenderId),
		Text:     messageBodyText(m.Content),
	}

	mi := c.extractMediaInfo(m)
	if mi == nil {
		return em
	}
	mi.ChatTitle = chatTitle
	em.MediaType = mi.MediaType
	em.FileName = mi.FileName
	em.FileSize = mi.FileSize
	em.AlbumID = mi.AlbumID

	// 只在文件真的躺在磁盘上时才给出引用：否则 HTML 里全是打不开的死链
	path := c.downloader.TargetPath(mi)
	if st, err := os.Stat(path); err == nil && !st.IsDir() {
		em.FilePath = path
	}
	return em
}

// messageBodyText 提取消息正文：纯文本取 text，媒体消息取 caption
func messageBodyText(content tdclient.MessageContent) string {
	if t, ok := content.(*tdclient.MessageText); ok {
		if t.Text != nil {
			return t.Text.Text
		}
		return ""
	}
	return captionText(content)
}

func reverseMessages(msgs []exportMessage) []exportMessage {
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs
}

// writeExport 落盘 JSON 与 HTML，并把媒体的绝对路径改写为相对导出文件的路径
func (c *Client) writeExport(spec ExportSpec, msgs []exportMessage, truncated bool) (*ExportResult, error) {
	dir := filepath.Join(c.downloader.DownloadPath(), exportDirName)
	if err := os.MkdirAll(dir, exportDirPerm); err != nil {
		return nil, fmt.Errorf("创建导出目录失败: %w", err)
	}

	now := time.Now()
	res := &ExportResult{
		Messages:     len(msgs),
		Truncated:    truncated,
		ChatTitle:    spec.ChatTitle,
		ExportedAtTS: now.Unix(),
	}

	// 绝对路径 → 相对导出文件的路径，导出目录整体搬走后 HTML 依然可用
	for i := range msgs {
		if msgs[i].MediaType != "" {
			res.MediaCount++
		}
		if msgs[i].FilePath == "" {
			continue
		}
		res.MediaOnDisk++
		if rel, err := filepath.Rel(dir, msgs[i].FilePath); err == nil {
			msgs[i].FilePath = filepath.ToSlash(rel)
		}
	}

	payload := exportPayload{
		ChatID:     spec.ChatID,
		ChatTitle:  spec.ChatTitle,
		ExportedAt: now.Unix(),
		Count:      len(msgs),
		Truncated:  truncated,
		Messages:   msgs,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("序列化导出内容失败: %w", err)
	}
	htmlData := []byte(renderExportHTML(payload))
	prefix := fmt.Sprintf("chat_%d_%s", spec.ChatID, now.Format("20060102_150405"))
	jsonFile, htmlFile, jsonPath, htmlPath, err := reserveExportFiles(dir, prefix)
	if err != nil {
		return nil, fmt.Errorf("预留导出文件失败: %w", err)
	}
	complete := false
	defer func() {
		_ = jsonFile.Close()
		_ = htmlFile.Close()
		if !complete {
			_ = os.Remove(jsonPath)
			_ = os.Remove(htmlPath)
		}
	}()
	if err := writeExportFile(jsonFile, data); err != nil {
		return nil, fmt.Errorf("写入 JSON 导出失败: %w", err)
	}
	if err := writeExportFile(htmlFile, htmlData); err != nil {
		return nil, fmt.Errorf("写入 HTML 导出失败: %w", err)
	}
	complete = true
	res.JSONPath = jsonPath
	res.HTMLPath = htmlPath

	c.logger.Info("导出完成：%d 条消息（%d 个媒体已在本地）→ %s", len(msgs), res.MediaOnDisk, htmlPath)
	return res, nil
}

// reserveExportFiles 独占创建一对同名 JSON/HTML 文件。CreateTemp 的随机后缀和 O_EXCL
// 保证同一聊天在同一秒并发导出时各自获得不同路径。
func reserveExportFiles(dir, prefix string) (
	jsonFile, htmlFile *os.File, jsonPath, htmlPath string, err error,
) {
	for {
		jsonFile, err = os.CreateTemp(dir, prefix+"_*.json")
		if err != nil {
			return nil, nil, "", "", err
		}
		jsonPath = jsonFile.Name()
		htmlPath = strings.TrimSuffix(jsonPath, ".json") + ".html"
		//nolint:gosec // htmlPath 由已创建的 jsonPath 派生，始终落在导出目录内
		htmlFile, err = os.OpenFile(htmlPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, exportFilePerm)
		if err == nil {
			return jsonFile, htmlFile, jsonPath, htmlPath, nil
		}
		_ = jsonFile.Close()
		_ = os.Remove(jsonPath)
		if os.IsExist(err) {
			continue
		}
		return nil, nil, "", "", err
	}
}

func writeExportFile(file *os.File, data []byte) error {
	if err := file.Chmod(exportFilePerm); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	return file.Close()
}

// renderExportHTML 生成自包含的 HTML（无外部资源引用，可离线打开）。
// 所有来自 Telegram 的文本都经 html.EscapeString 转义——消息内容是他人可控的。
func renderExportHTML(p exportPayload) string {
	var b strings.Builder
	title := p.ChatTitle
	if title == "" {
		title = fmt.Sprintf("聊天 %d", p.ChatID)
	}

	b.WriteString(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>` + html.EscapeString(title) + ` — 导出</title>
<style>
:root{color-scheme:light dark}
body{margin:0;padding:24px;font:15px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;background:#f5f5f7;color:#1d1d1f}
@media(prefers-color-scheme:dark){body{background:#161618;color:#f2f2f7}.msg{background:#1f1f22!important;border-color:#2c2c2e!important}}
header{max-width:820px;margin:0 auto 20px}
h1{font-size:22px;margin:0 0 4px}
.sub{color:#86868b;font-size:13px}
main{max-width:820px;margin:0 auto;display:flex;flex-direction:column;gap:10px}
.msg{background:#fff;border:1px solid #e5e5ea;border-radius:12px;padding:12px 14px}
.meta{color:#86868b;font-size:12px;margin-bottom:6px;display:flex;gap:10px;flex-wrap:wrap}
.text{white-space:pre-wrap;word-break:break-word}
.media{margin-top:8px}
.media img,.media video{max-width:100%;max-height:420px;border-radius:8px;display:block}
.missing{color:#86868b;font-size:13px;font-style:italic}
a{color:#0071e3}
</style></head><body>
<header>
<h1>` + html.EscapeString(title) + `</h1>
<div class="sub">`)
	fmt.Fprintf(&b, "聊天 %d · %d 条消息 · 导出于 %s",
		p.ChatID, p.Count, time.Unix(p.ExportedAt, 0).Format("2006-01-02 15:04:05"))
	if p.Truncated {
		b.WriteString(" · <strong>已按上限截断</strong>")
	}
	b.WriteString("</div></header>\n<main>\n")

	for i := range p.Messages {
		writeExportMessageHTML(&b, &p.Messages[i])
	}

	b.WriteString("</main></body></html>\n")
	return b.String()
}

// writeExportMessageHTML 渲染单条消息
func writeExportMessageHTML(b *strings.Builder, m *exportMessage) {
	b.WriteString(`<div class="msg"><div class="meta"><span>`)
	b.WriteString(time.Unix(m.Date, 0).Format("2006-01-02 15:04"))
	b.WriteString("</span>")
	if m.SenderID != 0 {
		fmt.Fprintf(b, "<span>发送者 %d</span>", m.SenderID)
	}
	fmt.Fprintf(b, "<span>#%d</span></div>", m.ID)

	if m.Text != "" {
		b.WriteString(`<div class="text">` + html.EscapeString(m.Text) + `</div>`)
	}
	if m.MediaType == "" {
		b.WriteString("</div>\n")
		return
	}

	b.WriteString(`<div class="media">`)
	switch {
	case m.FilePath == "":
		b.WriteString(`<div class="missing">[` + html.EscapeString(m.MediaType) + " " +
			html.EscapeString(m.FileName) + " — 尚未下载]</div>")
	case isImageExport(m):
		src := html.EscapeString(m.FilePath)
		b.WriteString(`<img loading="lazy" src="` + src + `" alt="` + html.EscapeString(m.FileName) + `">`)
	case isVideoExport(m):
		b.WriteString(`<video controls preload="none" src="` + html.EscapeString(m.FilePath) + `"></video>`)
	default:
		b.WriteString(`<a href="` + html.EscapeString(m.FilePath) + `">` + html.EscapeString(m.FileName) + `</a>`)
	}
	b.WriteString("</div></div>\n")
}

// isImageExport/isVideoExport 决定 HTML 里用 <img>/<video> 还是普通链接
func isImageExport(m *exportMessage) bool {
	switch strings.ToLower(filepath.Ext(m.FileName)) {
	case ".jpg", ".jpeg", ".png", ".gif", extWebp:
		return true
	}
	return false
}

func isVideoExport(m *exportMessage) bool {
	switch strings.ToLower(filepath.Ext(m.FileName)) {
	case extMp4, extWebm, ".mov":
		return true
	}
	return false
}
