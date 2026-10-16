package agent

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"github.com/RC-CHN/ARTEX/db"
	"github.com/RC-CHN/ARTEX/norma/harness"
	"github.com/RC-CHN/ARTEX/norma/llm"
	"github.com/RC-CHN/ARTEX/resourcelib"
)

type resourceSnapshotKey struct{}
type resourceUpdatesKey struct{}

func WithResourceUpdates(ctx context.Context, source func(context.Context) ([]resourcelib.UpdateState, error)) context.Context {
	return context.WithValue(ctx, resourceUpdatesKey{}, source)
}

// Delivery is serialized by the harness. History markers handle resumes;
// in-run memory handles compaction. Errors preserve the next retry and never
// abort tools or change a task pin. Poll at most once per five seconds per run.
func resourceNoticeContext(ctx context.Context, emit func(db.Activity)) context.Context {
	source, _ := ctx.Value(resourceUpdatesKey{}).(func(context.Context) ([]resourcelib.UpdateState, error))
	if source == nil {
		return ctx
	}
	seen := map[int64]resourcelib.UpdateState{}
	var last time.Time
	return harness.WithTurnNotices(ctx, func(ctx context.Context, history []llm.Message) string {
		if time.Since(last) < 5*time.Second {
			return ""
		}
		last = time.Now()
		queryCtx, cancel := context.WithTimeout(ctx, time.Second)
		states, err := source(queryCtx)
		cancel()
		if err != nil {
			return ""
		}
		var notices []string
		for _, state := range states {
			previous, exists := seen[state.ResourceID]
			seen[state.ResourceID] = state
			if (exists && previous == state) || (!exists && state.Enabled && state.LatestID == state.PinnedID) {
				continue
			}
			marker := fmt.Sprintf("[resource-update:%d:%d:%d:%t]", state.ResourceID, state.PinnedID, state.LatestID, state.Enabled)
			duplicate := false
			for _, message := range history {
				if message.Role == llm.RoleUser && strings.Contains(message.Text(), marker) {
					duplicate = true
					break
				}
			}
			if duplicate {
				continue
			}
			notices = append(notices, fmt.Sprintf("%s 自有资源 #%d：任务当前 version_id=%d，最新 version_id=%d，可用=%t。", marker, state.ResourceID, state.PinnedID, state.LatestID, state.Enabled))
		}
		if len(notices) == 0 {
			return ""
		}
		text := "【资源版本通知】\n" + strings.Join(notices, "\n") + "\n这是资源库状态通知；不会中断工具或替换已返回的旧文件路径。用 use_resource 查看当前版本；需要切换时明确提供目标 version_id 与 expected_version_id。先评估兼容性，无需为了通知而重启任务。"
		if emit != nil {
			emit(db.Activity{Kind: "resource_update", Summary: "自有资源版本更新", Detail: text})
		}
		return text
	})
}

// A fixed task snapshot is supplied as a user-context message, leaving the
// system/tool prefix unchanged. It is never rebuilt during model turns.
func WithResourceSnapshot(ctx context.Context, snapshot string) context.Context {
	return context.WithValue(ctx, resourceSnapshotKey{}, snapshot)
}
func resourceSnapshotInput(ctx context.Context, messages []llm.Message, input string) string {
	snapshot, _ := ctx.Value(resourceSnapshotKey{}).(string)
	if snapshot == "" {
		return input
	}
	sum := sha256.Sum256([]byte(snapshot))
	marker := fmt.Sprintf("[task-resource-snapshot:%x]", sum[:16])
	for _, message := range messages {
		if message.Role != llm.RoleUser {
			continue
		}
		for _, block := range message.Content {
			if block.Type == llm.BlockText && strings.Contains(block.Text, marker) {
				return input
			}
		}
	}
	return marker + "\n任务已选择以下自有资源及固定版本。仅注入元数据，正文与文件通过 use_resource 按需读取；这些是自有资源，不属于测试目标。全局更新不会自动替换固定版本。\n" + snapshot + "\n\n" + input
}
