package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ReleasesAPI 是项目的最新发布查询端点（公开、无鉴权）
const ReleasesAPI = "https://api.github.com/repos/Heartcoolman/Tg-Down/releases/latest"

const updateHTTPTimeout = 10 * time.Second

// Release 是一次更新检查的结果
type Release struct {
	Tag     string `json:"tag"`
	URL     string `json:"url"`
	Notes   string `json:"notes,omitempty"`
	Version string `json:"version"` // 规范化后的版本号（无 v 前缀）
}

// CheckUpdate 查询 GitHub 最新 release；current 为当前版本（dev 视为 0.0.0）
func CheckUpdate(current string) (*Release, error) {
	client := &http.Client{Timeout: updateHTTPTimeout}
	ctx, cancel := context.WithTimeout(context.Background(), updateHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ReleasesAPI, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("查询更新失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("查询更新失败: HTTP %d", resp.StatusCode)
	}
	var body struct {
		TagName    string `json:"tag_name"`
		HTMLURL    string `json:"html_url"`
		Name       string `json:"name"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("解析发布信息失败: %w", err)
	}
	if body.TagName == "" {
		return nil, fmt.Errorf("发布信息缺少 tag_name")
	}
	r := &Release{Tag: body.TagName, URL: body.HTMLURL, Notes: body.Name}
	r.Version = strings.TrimPrefix(body.TagName, "v")
	if NewerVersion(r.Version, current) && !body.Prerelease {
		return r, nil
	}
	return nil, nil // 有响应但无需更新：nil, nil
}

// NewerVersion 判断 candidate 是否严格新于 current；非 semver 形态按段比较，段数不足补零。
// 无法解析时返回 false（宁可不提示，不误报）。
func NewerVersion(candidate, current string) bool {
	c, okc := parseVersion(candidate)
	u, oku := parseVersion(current)
	if !okc || !oku {
		return false
	}
	for i := range max(len(c), len(u)) {
		var cv, uv int64
		if i < len(c) {
			cv = c[i]
		}
		if i < len(u) {
			uv = u[i]
		}
		if cv != uv {
			return cv > uv
		}
	}
	return false
}

func parseVersion(v string) ([]int64, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if v == "" || v == "dev" {
		return []int64{0, 0, 0}, true
	}
	parts := strings.Split(v, ".")
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		num := p
		// 剥掉 1.2.0-beta1 这类后缀
		if i := strings.IndexAny(p, "-+"); i >= 0 {
			num = p[:i]
		}
		n, err := strconv.ParseInt(num, 10, 32)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}
