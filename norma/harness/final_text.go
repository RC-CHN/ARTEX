package harness

import (
	"strings"

	"github.com/RC-CHN/ARTEX/norma/llm"
)

// These reminders belong to one model request, not the resumed conversation.
const finalTextOnlyPrompt = "上一轮没有最终文字答复。本轮只用文字向用户汇报已有结果、可核对的记录 ID 或产物路径、未完成项与阻碍；没有完成也请明确说明。禁止调用任何工具，禁止继续执行工作，不推测或编造结果。"

const continueAfterEmptyReplyPrompt = "上一轮没有向用户给出文字答复。这不代表预算耗尽，也不要求你收尾。请根据原请求继续处理，仍可正常调用工具；需要继续工作就继续，已完成或遇到阻碍则向用户说明已有结果、未完成项与阻碍。不要重复已经完成的操作，不推测或编造结果。本提醒仅适用于当前请求。"

// v0.14.0 persisted this exact SDK-generated user message after an empty
// assistant response. Exclude only that shape from the Main Agent's working
// history; keep raw transcript files and all other user messages unchanged.
const legacyFinalTextOnlyPrompt = "上一轮正常结束但没有最终文字答复。本轮只用文字向用户汇报已有结果、可核对的记录 ID 或产物路径、未完成项与阻碍；没有完成也请明确说明。禁止调用任何工具，禁止继续执行工作，不推测或编造结果。"

func withoutLegacyFinalTextPrompt(msgs []llm.Message) []llm.Message {
	out := make([]llm.Message, 0, len(msgs))
	for i, m := range msgs {
		if i > 0 && i+1 < len(msgs) && m.Role == llm.RoleUser && len(m.Content) == 1 && m.Content[0].Type == llm.BlockText && m.Content[0].Text == legacyFinalTextOnlyPrompt {
			prev := msgs[i-1]
			if prev.Role == llm.RoleAssistant && strings.TrimSpace(prev.Text()) == "" && len(prev.ToolUses()) == 0 {
				continue
			}
		}
		out = append(out, m)
	}
	return out
}
