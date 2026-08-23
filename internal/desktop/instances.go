package desktop

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
)

// LocalInstanceID 是内置本地实例的保留 ID；远程实例不得占用
const LocalInstanceID = "local"

// Instance 描述一个可管理的下载实例。本地实例不入注册表，由壳层动态合成。
type Instance struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	URL   string `json:"url"`
	Token string `json:"token,omitempty"`
}

// instanceFile 是 instances.json 的磁盘结构
type instanceFile struct {
	Instances []*Instance `json:"instances"`
	Selected  string      `json:"selected"`
}

// Registry 管理远程实例列表与当前选中项，串行化落盘。
// Token 仅存于本文件与壳层内存，绝不进入前端。
type Registry struct {
	mu   sync.Mutex
	path string
	data instanceFile
}

// OpenRegistry 从 path 加载注册表；文件不存在时返回空表
func OpenRegistry(path string) (*Registry, error) {
	r := &Registry{path: path}
	data, err := os.ReadFile(path) //nolint:gosec // path 由应用数据目录拼出，非用户输入
	if os.IsNotExist(err) {
		r.data.Selected = LocalInstanceID
		return r, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取实例注册表失败: %w", err)
	}
	if err := json.Unmarshal(data, &r.data); err != nil {
		return nil, fmt.Errorf("解析实例注册表失败: %w", err)
	}
	if r.data.Selected == "" {
		r.data.Selected = LocalInstanceID
	}
	r.normalizeLocked()
	return r, nil
}

// normalizeLocked 清洗：剔除 local 保留 ID、去重、URL 规范化、排序
func (r *Registry) normalizeLocked() {
	seen := make(map[string]bool, len(r.data.Instances))
	kept := r.data.Instances[:0]
	for _, in := range r.data.Instances {
		in.ID = strings.TrimSpace(in.ID)
		in.Name = strings.TrimSpace(in.Name)
		in.URL = NormalizeBaseURL(in.URL)
		if in.ID == "" || in.ID == LocalInstanceID || seen[in.ID] || in.URL == "" {
			continue
		}
		if in.Name == "" {
			in.Name = in.URL
		}
		seen[in.ID] = true
		kept = append(kept, in)
	}
	r.data.Instances = kept
	if r.data.Selected != LocalInstanceID && !seen[r.data.Selected] {
		r.data.Selected = LocalInstanceID
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].Name < kept[j].Name })
}

func (r *Registry) saveLocked() error {
	data, err := json.MarshalIndent(&r.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}

// List 返回全部远程实例（副本）
func (r *Registry) List() []Instance {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Instance, len(r.data.Instances))
	for i, in := range r.data.Instances {
		out[i] = *in
	}
	return out
}

// Get 按 ID 取实例；不存在返回 false
func (r *Registry) Get(id string) (Instance, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, in := range r.data.Instances {
		if in.ID == id {
			return *in, true
		}
	}
	return Instance{}, false
}

// Add 新增实例并返回；name 为空时用 URL 充当名称
func (r *Registry) Add(name, rawURL string) (Instance, error) {
	u := NormalizeBaseURL(rawURL)
	if u == "" {
		return Instance{}, fmt.Errorf("实例地址无效")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	id, err := newID()
	if err != nil {
		return Instance{}, err
	}
	in := &Instance{ID: id, Name: strings.TrimSpace(name), URL: u}
	if in.Name == "" {
		in.Name = u
	}
	r.data.Instances = append(r.data.Instances, in)
	r.normalizeLocked()
	if err := r.saveLocked(); err != nil {
		return Instance{}, err
	}
	return *in, nil
}

// Update 就地修改实例；token 为 nil 表示保持不变，非 nil（可为空串）表示覆写
func (r *Registry) Update(id, name, rawURL string, token *string) (Instance, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var target *Instance
	for _, in := range r.data.Instances {
		if in.ID == id {
			target = in
			break
		}
	}
	if target == nil {
		return Instance{}, fmt.Errorf("实例不存在: %s", id)
	}
	if name != "" {
		target.Name = strings.TrimSpace(name)
	}
	if rawURL != "" {
		u := NormalizeBaseURL(rawURL)
		if u == "" {
			return Instance{}, fmt.Errorf("实例地址无效")
		}
		target.URL = u
	}
	if token != nil {
		target.Token = strings.TrimSpace(*token)
	}
	r.normalizeLocked()
	if err := r.saveLocked(); err != nil {
		return Instance{}, err
	}
	return *target, nil
}

// Delete 删除实例；若删除的是选中项则回落本地
func (r *Registry) Delete(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := r.data.Instances[:0]
	found := false
	for _, in := range r.data.Instances {
		if in.ID == id {
			found = true
			continue
		}
		kept = append(kept, in)
	}
	if !found {
		return fmt.Errorf("实例不存在: %s", id)
	}
	r.data.Instances = kept
	if r.data.Selected == id {
		r.data.Selected = LocalInstanceID
	}
	r.normalizeLocked()
	return r.saveLocked()
}

// Selected 返回当前选中的实例 ID（local 或远程 ID）
func (r *Registry) Selected() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.data.Selected
}

// Select 切换选中项；id 必须为 local 或已存在的远程实例
func (r *Registry) Select(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id != LocalInstanceID {
		found := false
		for _, in := range r.data.Instances {
			if in.ID == id {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("实例不存在: %s", id)
		}
	}
	r.data.Selected = id
	return r.saveLocked()
}

// newID 生成 8 字节随机十六进制 ID
func newID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成实例 ID 失败: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// NormalizeBaseURL 规范化实例地址：去空白与结尾斜杠，校验 scheme/host。
// 返回空串表示输入不可用。
func NormalizeBaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	if u.Path != "" && u.Path != "/" {
		return ""
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host, "/") + "/"
}
