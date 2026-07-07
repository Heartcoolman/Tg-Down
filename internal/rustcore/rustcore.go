// Package rustcore provides an optional bridge to the Rust business-rule helper.
//
// The Go implementation remains authoritative and is used as a fallback whenever
// the helper is not installed or returns an error.
package rustcore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

const (
	helperName      = "tg-down-core"
	helperTimeout   = 3 * time.Second
	disableEnv      = "TG_DOWN_RUST_CORE"
	helperPathEnv   = "TG_DOWN_RUST_CORE_BIN"
	disableEnvValue = "0"
	planMediaPathOp = "plan_media_path"
	// protocolVersion 是 Go↔helper 的协议版本。响应版本不一致（含旧版 helper 不回传版本时的 0）
	// 一律视为不兼容并回退 Go 逻辑，避免陈旧 helper 用过时规则静默覆盖新的路径逻辑。
	protocolVersion = 1
)

var helperState struct {
	once sync.Once
	path string
	err  error
}

// MediaInfo mirrors the Rust core media input shape.
type MediaInfo struct {
	MessageID int64  `json:"message_id"`
	TDFileID  int32  `json:"td_file_id"`
	MediaType string `json:"media_type"`
	FileName  string `json:"file_name,omitempty"`
	FileSize  int64  `json:"file_size"`
	MimeType  string `json:"mime_type"`
	ChatID    int64  `json:"chat_id"`
	TaskID    string `json:"task_id,omitempty"`
}

// PathPlan is the Rust helper result for a media destination path.
type PathPlan struct {
	ProtocolVersion int    `json:"protocol_version"`
	Directory       string `json:"directory"`
	FileName        string `json:"file_name"`
	FilePath        string `json:"file_path"`
}

type planMediaPathRequest struct {
	Op              string    `json:"op"`
	ProtocolVersion int       `json:"protocol_version"`
	DownloadPath    string    `json:"download_path"`
	ClassifyByType  bool      `json:"classify_by_type"`
	Media           MediaInfo `json:"media"`
}

// PlanMediaPath asks the Rust helper to plan a download path.
func PlanMediaPath(ctx context.Context, downloadPath string, classifyByType bool, media MediaInfo) (PathPlan, error) {
	helperPath, err := helper()
	if err != nil {
		return PathPlan{}, err
	}

	req := planMediaPathRequest{
		Op:              planMediaPathOp,
		ProtocolVersion: protocolVersion,
		DownloadPath:    downloadPath,
		ClassifyByType:  classifyByType,
		Media:           media,
	}
	data, err := json.Marshal(req)
	if err != nil {
		return PathPlan{}, fmt.Errorf("编码 Rust helper 请求失败: %w", err)
	}

	cmdCtx, cancel := context.WithTimeout(ctx, helperTimeout)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, helperPath)
	cmd.Stdin = bytes.NewReader(data)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		if cmdCtx.Err() != nil {
			return PathPlan{}, cmdCtx.Err()
		}
		return PathPlan{}, fmt.Errorf("Rust helper 执行失败: %w: %s", err, stderr.String())
	}

	var plan PathPlan
	if err := json.Unmarshal(out, &plan); err != nil {
		return PathPlan{}, fmt.Errorf("解析 Rust helper 响应失败: %w", err)
	}
	if plan.ProtocolVersion != protocolVersion {
		return PathPlan{}, fmt.Errorf("Rust helper 协议版本不兼容: 期望 %d, 实际 %d", protocolVersion, plan.ProtocolVersion)
	}
	if plan.Directory == "" || plan.FileName == "" || plan.FilePath == "" {
		return PathPlan{}, fmt.Errorf("Rust helper 返回了空路径")
	}
	return plan, nil
}

func helper() (string, error) {
	helperState.once.Do(func() {
		helperState.path, helperState.err = discoverHelper()
	})
	return helperState.path, helperState.err
}

// discoverHelper 定位可信的 helper 二进制。为规避不可信搜索路径执行（CWE-427），
// 只信任显式的 TG_DOWN_RUST_CORE_BIN 或与主程序同目录的 helper；绝不探测当前工作目录或 PATH，
// 否则在含攻击者植入的 tg-down-core 的目录中运行会被静默执行。helper 缺失时调用方回退纯 Go 逻辑。
func discoverHelper() (string, error) {
	if os.Getenv(disableEnv) == disableEnvValue {
		return "", fmt.Errorf("Rust helper 已通过 %s=%s 禁用", disableEnv, disableEnvValue)
	}
	if path := os.Getenv(helperPathEnv); path != "" {
		if isExecutable(path) {
			return path, nil
		}
		return "", fmt.Errorf("%s 指向的文件不可执行: %s", helperPathEnv, path)
	}

	if exe, err := os.Executable(); err == nil {
		if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			exe = resolved
		}
		candidate := filepath.Join(filepath.Dir(exe), executableName(helperName))
		if isExecutable(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("未找到 Rust helper %s（仅信任 %s 或与主程序同目录）", helperName, helperPathEnv)
}

func executableName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode().Perm()&0111 != 0
}
