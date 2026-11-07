package bot

import (
	"regexp"
	"strings"
)

// Chat models sometimes announce an action ("好，我建个定时任务，稍等") and end the turn without calling the tool, so the
// user is left with a promise nobody keeps. promisesAction spots such replies so the loop can push the model once to
// actually call the tool (or give the real answer) before anything is sent.
var promiseRE = regexp.MustCompile(`稍等|等我一下|等我下|建完|做完|弄完|搞定后|完成后[^。！？\n]{0,8}(告诉|通知|发)|` +
	`(我|这就|马上|现在|立刻|立即)(来|去|就|先)?(给你|帮你|为你)?(重新|再)?(建|创建|设置|设定|查一下|查查|搜索|搜一下|生成|写|算|调用|试一下|试试)`)

func promisesAction(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	for _, loc := range promiseRE.FindAllStringIndex(text, -1) {
		rest := []rune(text[loc[1]:])
		if len(rest) > 0 && (rest[0] == '了' || rest[0] == '过' || rest[0] == '的') { // "我写了…" reports, it does not promise
			continue
		}
		return true
	}
	return false
}

const noActionNudge = "你刚才的回复在说要去做某件事（例如建任务、查资料、生成文件），但没有调用任何工具，所以什么都没有发生。" +
	"如果需要用工具，现在就调用；做完并确认结果之后，再用一句话告诉用户真实结果。" +
	"如果其实不需要工具，就直接给出最终答复，不要再说“稍等”“我去做”之类的话。"
