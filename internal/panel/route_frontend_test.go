package panel

import (
	"os"
	"strings"
	"testing"
)

// 路由聚合面板的结构契约：后端接口复用 /panel/api/config，前端必须
//   1) 有挂载点与保存按钮；
//   2) 保存时把 routes 段写回整份 config（不能只发 routes，否则会清掉别的键）；
//   3) 有拖拽排序入口（顺序即优先级）。
// 这是 UI 与配置层之间的接口约定，改坏任意一条都会让功能静默失效，
// 而 app.js 是内嵌资源、没有类型检查兜底。
func TestRoutePanelWiring(t *testing.T) {
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	h, j := string(html), string(js)

	for _, want := range []string{`id="rtList"`, `id="rtSave"`, `id="rtEnabled"`, `id="rtMaxAttempts"`} {
		if !strings.Contains(h, want) {
			t.Fatalf("index.html 缺少路由聚合挂载点 %s", want)
		}
	}

	checks := []struct{ name, needle string }{
		{"读取配置", `api('config')`},
		{"写回整份配置", `cfg.routes =`},
		{"保存走 config 接口", `api('config', { method: 'POST'`},
		{"拖拽排序", `addEventListener('drop'`},
		{"目标候选来自模型清单", `mdAll.map`},
		{"聚合名进请求体", `name: String(it.name || '').trim()`},
		// 目标过多时必须能搜索：输入框 + 大小写不敏感的过滤 + 已添加项剔除
		{"搜索输入框", `class="rt-pick-q"`},
		{"过滤实现", `rtPickCandidates`},
		{"大小写不敏感", `toLowerCase()`},
		{"已添加项剔除", `have[t.realm + ':' + t.model]`},
		{"键盘选择", `ArrowDown`},
		// 目标行三列：实时状态（读锁表）/ 倍率（复用模型页口径）/ 上下文
		{"状态解析", `function rtStatusOf`},
		{"锁表解锁时刻", `unlock_at`},
		{"倍率复用模型页", `rateCell(m)`},
		{"上下文列", `context_length`},
		{"三列渲染", `function rtMetaHtml`},
		// 下拉必须是 fixed 全局弹层：卡片容器带 overflow，内联 absolute 会被裁掉
		{"弹层挂 body", `id = 'rtPickBox'`},
		{"弹层 fixed 定位", `position:fixed;z-index:200`},
		// 回归：滚动/失焦都不得收起弹层（弹层自身可滚动，收起会把自己滚没）
		{"滚动只跟随", `function rtPickOnScroll`},
		{"跟随重定位", `function rtPickPlace`},
	}
	for _, c := range checks {
		if !strings.Contains(j, c.needle) {
			t.Fatalf("app.js 缺少「%s」实现（找 %q）", c.name, c.needle)
		}
	}

	// 保存必须发生在「重新读取最新配置」之后，否则并发改配置会被覆盖。
	if strings.Index(j, `const d = await api('config');`) > strings.Index(j, `cfg.routes =`) {
		t.Fatal("保存前应先 GET 最新配置再改 routes（避免覆盖其它字段）")
	}

	// 反向断言：这两个监听一旦回来，下拉就会「滚一下/点一下全没了」（真实回归过）。
	for _, bad := range []string{`addEventListener('scroll', rtPickHide`, `addEventListener('focusout'`} {
		if strings.Contains(j, bad) {
			t.Fatalf("弹层不应因滚动/失焦而收起，但发现 %q", bad)
		}
	}
}
