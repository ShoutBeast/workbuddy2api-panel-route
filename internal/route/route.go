// Package route 实现「模型路由聚合」：把对外暴露的聚合模型名解析成一条按优先级
// 排列的目标链（realm + 裸模型名），并提供推进判定与配置校验。
//
// 设计约束（见 docs/模型路由聚合-技术方案-v2.md）：
//   - 纯策略包：不 import server/pool，不持请求状态，便于单测与多入口（chat/messages/
//     responses）复用；
//   - 推进语义 = 「当前目标已无可用候选，或轮转耗尽」时才落到下一个目标。这与三家
//     参考实现同构：litellm 只保留 healthy deployment 里的 min(order) 档、
//     new-api 用 priorities[retry] 逐档下移、one-api 首次限最高 priority 之后放开；
//   - 流式能否推进由调用方判定（upstream.HeadVetoError = 尚未向客户端写任何字节）。
package route

import (
	"errors"
	"fmt"
	"strings"

	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// Target 链上一环。
type Target struct {
	Realm string // "cn" / "global"
	Model string // 裸模型名（不含 realm 前缀）
}

// ConfigTarget 配置里的目标条目。
type ConfigTarget struct {
	Realm string `json:"realm"`
	Model string `json:"model"`
}

// Config 配置里的单条聚合。
type Config struct {
	Name    string         `json:"name"`
	Enabled *bool          `json:"enabled,omitempty"` // nil = 启用
	Targets []ConfigTarget `json:"targets"`
	// On 允许推进的错误类；空 = 默认集（见 DefaultOn）。
	On []string `json:"on,omitempty"`
}

// Settings 配置里的 routes 整块。
type Settings struct {
	Enabled     *bool    `json:"enabled,omitempty"` // nil = 启用
	MaxAttempts int      `json:"max_attempts,omitempty"`
	Items       []Config `json:"items"`
}

// Item 归一化后的单条聚合。
type Item struct {
	Name    string
	Targets []Target
	On      map[upstream.ErrKind]bool
}

// Snapshot 归一化结果，经 livecfg 热替换。
type Snapshot struct {
	Enabled     bool
	MaxAttempts int             // 0 = 按链长推导（见 DefaultBudget）
	Items       map[string]*Item // 键 = 小写聚合名
}

// Chain 一次请求用的只读链视图。
type Chain struct {
	Name    string
	Targets []Target
	Budget  int // 本次请求允许的上游出站次数上限
	on      map[upstream.ErrKind]bool
}

// Options 归一化时用于交叉校验的外部事实。
type Options struct {
	// Hidden 已归一化的黑名单键（"cn:x"/"global:x" 全量形式，见 livecfg.NormalizeHiddenModels）。
	Hidden map[string]struct{}
	// KnownModels 已知真实模型名（小写裸名）。nil = 跳过同名冲突校验。
	KnownModels map[string]struct{}
}

// 聚合名禁止字符：":" 与 realm 前缀语法冲突；"=" 与 "," 是历史扁平格式的分隔符；
// 空白会让配置手写/日志对齐歧义。
const forbiddenNameChars = ":,= \t\r\n"

const maxNameLen = 64

// 聚合链长度上限：防御误配把一条链写成二十环，把单请求放大成二十次上游调用。
const maxTargetsPerRoute = 8

// maxAttemptsCeiling 全局尝试预算的硬上限。
const maxAttemptsCeiling = 20

// onNames 是配置里 on 字段的稳定词表。刻意不复用 ErrKind.String()：
// 那是观测用词（日志/面板），改动会静默改变用户配置的含义。
var onNames = map[string]upstream.ErrKind{
	"soft_rate":     upstream.ErrSoftRate,
	"server":        upstream.ErrServer,
	"upstream_busy": upstream.ErrUpstreamBusy,
	"not_found":     upstream.ErrNotFound,
	"hard_credit":   upstream.ErrHardCredit,
	"account_fault": upstream.ErrAccountFault,
	"model_blocked": upstream.ErrModelBlocked,
}

// AdvanceableNames 返回可用于 on 字段的全部词，供面板 UI 展示。
func AdvanceableNames() []string {
	out := make([]string, 0, len(onNames))
	for _, n := range []string{
		"soft_rate", "server", "upstream_busy", "not_found",
		"hard_credit", "account_fault", "model_blocked",
	} {
		out = append(out, n)
	}
	return out
}

// DefaultOn 默认允许推进的错误类。
//
// 刻意排除：ErrSessionDead（需重新登录，换模型救不了）、ErrContentBlocked /
// ErrPromptTooLong / ErrBadParams / ErrImageInvalid / ErrModelParamInvalid /
// ErrToolMismatch（请求级错误，换目标只会掩盖真实原因）、ErrWafBlock（IP 级封禁，
// 换目标等于放大风控暴露）。
func DefaultOn() map[upstream.ErrKind]bool {
	out := make(map[upstream.ErrKind]bool, len(onNames))
	for _, k := range onNames {
		out[k] = true
	}
	return out
}

// DefaultBudget 单条链允许的出站次数：min(2×链长, 6)，至少 1。
//
// 2× 的来源：一个目标内通常「换一次号」就够判断是该目标整体不可用；
// 6 的上限来自现有 MaxRotate=3 × 2 档的经验值，避免长链把单请求放大成十几次。
func DefaultBudget(n int) int {
	if n <= 0 {
		return 1
	}
	b := 2 * n
	if b > 6 {
		b = 6
	}
	if b < 1 {
		b = 1
	}
	return b
}

// Lookup 按聚合名取链。name 允许带 "cn:"/"global:" 前缀（客户端可能加），
// 前缀不改变结果：聚合不属于任何 realm，链上目标自带 realm。
func (s *Snapshot) Lookup(name string) (*Chain, bool) {
	if s == nil || !s.Enabled || len(s.Items) == 0 {
		return nil, false
	}
	key := normalizeName(strings.ToLower(strings.TrimSpace(name)))
	if prefix, bare, cut := strings.Cut(key, ":"); cut && (prefix == "cn" || prefix == "global") {
		key = bare
	}
	it, ok := s.Items[key]
	if !ok || len(it.Targets) == 0 {
		return nil, false
	}
	budget := s.MaxAttempts
	if budget <= 0 {
		budget = DefaultBudget(len(it.Targets))
	}
	return &Chain{Name: it.Name, Targets: it.Targets, Budget: budget, on: it.On}, true
}

// ShouldAdvance 判定「当前目标已耗尽时」是否允许推进到下一目标。
// 调用方必须只在本目标轮转结束（或本地无候选）之后调用：目标内先换号是硬语义。
func (c *Chain) ShouldAdvance(kind upstream.ErrKind) bool {
	if c == nil {
		return false
	}
	if len(c.on) == 0 {
		return DefaultOn()[kind]
	}
	return c.on[kind]
}

// TargetList 返回链上目标（便于日志/测试只读访问）。
func (c *Chain) TargetList() []Target {
	if c == nil {
		return nil
	}
	return c.Targets
}

// normalizeName 去空白并小写。聚合名的展示大小写保留在 Item.Name。
func normalizeName(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// Normalize 校验并归一化 routes 配置。
//
// 返回的 error 聚合了全部条目问题（errors.Join），面板可逐条展示；任一错误存在时
// 调用方**不得**落盘/热应用（与既有 SaveConfig 语义一致）。
func Normalize(s Settings, opt Options) (Snapshot, error) {
	enabled := s.Enabled == nil || *s.Enabled
	maxAttempts := s.MaxAttempts
	var errs []error
	if maxAttempts < 0 || maxAttempts > maxAttemptsCeiling {
		errs = append(errs, fmt.Errorf("routes.max_attempts 需在 0..%d 之间，实际 %d", maxAttemptsCeiling, maxAttempts))
		maxAttempts = 0
	}

	// 第一遍：收集全部聚合名，供「目标不得指向聚合名」的交叉校验。
	names := make(map[string]struct{}, len(s.Items))
	for i, it := range s.Items {
		name := strings.TrimSpace(it.Name)
		if name == "" {
			errs = append(errs, fmt.Errorf("routes.items[%d]: name 不能为空", i))
			continue
		}
		key := normalizeName(name)
		if _, dup := names[key]; dup {
			errs = append(errs, fmt.Errorf("routes.items[%d]: 聚合名 %q 重复", i, name))
			continue
		}
		names[key] = struct{}{}
	}

	out := Snapshot{Enabled: enabled, MaxAttempts: maxAttempts, Items: make(map[string]*Item, len(s.Items))}
	for i, it := range s.Items {
		name := strings.TrimSpace(it.Name)
		if name == "" {
			continue
		}
		key := normalizeName(name)
		if _, dup := out.Items[key]; dup {
			continue
		}
		if bad, ok := firstForbidden(name); ok {
			errs = append(errs, fmt.Errorf("routes.items[%d] %q: 名称不能包含 %q", i, name, bad))
			continue
		}
		if len([]rune(name)) > maxNameLen {
			errs = append(errs, fmt.Errorf("routes.items[%d] %q: 名称最长 %d 字符", i, name, maxNameLen))
			continue
		}
		if _, clash := opt.KnownModels[key]; clash {
			errs = append(errs, fmt.Errorf("routes.items[%d] %q: 与真实模型同名，会遮蔽该模型", i, name))
			continue
		}
		if len(it.Targets) == 0 {
			errs = append(errs, fmt.Errorf("routes.items[%d] %q: targets 不能为空", i, name))
			continue
		}
		if len(it.Targets) > maxTargetsPerRoute {
			errs = append(errs, fmt.Errorf("routes.items[%d] %q: 目标最多 %d 个，实际 %d",
				i, name, maxTargetsPerRoute, len(it.Targets)))
			continue
		}

		on, onErr := parseOn(it.On)
		if onErr != nil {
			errs = append(errs, fmt.Errorf("routes.items[%d] %q: %w", i, name, onErr))
			continue
		}

		targets := make([]Target, 0, len(it.Targets))
		seen := make(map[string]struct{}, len(it.Targets))
		ok := true
		for j, t := range it.Targets {
			realm := strings.ToLower(strings.TrimSpace(t.Realm))
			model := strings.TrimSpace(t.Model)
			if realm != "cn" && realm != "global" {
				errs = append(errs, fmt.Errorf("routes.items[%d] %q targets[%d]: realm 必须是 cn 或 global，实际 %q",
					i, name, j, t.Realm))
				ok = false
				continue
			}
			if model == "" {
				errs = append(errs, fmt.Errorf("routes.items[%d] %q targets[%d]: model 不能为空", i, name, j))
				ok = false
				continue
			}
			if strings.Contains(model, ":") {
				errs = append(errs, fmt.Errorf("routes.items[%d] %q targets[%d]: model 不要带 realm 前缀（用 realm 字段表达）", i, name, j))
				ok = false
				continue
			}
			mkey := normalizeName(model)
			if _, isRoute := names[mkey]; isRoute {
				errs = append(errs, fmt.Errorf("routes.items[%d] %q targets[%d]: 目标 %q 是另一个聚合名，禁止递归展开",
					i, name, j, model))
				ok = false
				continue
			}
			if _, hidden := opt.Hidden[realm+":"+mkey]; hidden {
				errs = append(errs, fmt.Errorf("routes.items[%d] %q targets[%d]: 目标 %q 在黑名单中，聚合不得绕过黑名单",
					i, name, j, model))
				ok = false
				continue
			}
			dedupKey := realm + ":" + mkey
			if _, dup := seen[dedupKey]; dup {
				continue // 重复目标静默去掉，保留首次出现位置
			}
			seen[dedupKey] = struct{}{}
			targets = append(targets, Target{Realm: realm, Model: model})
		}
		if !ok || len(targets) == 0 {
			continue
		}
		out.Items[key] = &Item{Name: name, Targets: targets, On: on}
	}

	if len(errs) > 0 {
		return Snapshot{}, errors.Join(errs...)
	}
	return out, nil
}

// parseOn 解析 on 词表；空 = 默认集。
func parseOn(in []string) (map[upstream.ErrKind]bool, error) {
	if len(in) == 0 {
		return DefaultOn(), nil
	}
	out := make(map[upstream.ErrKind]bool, len(in))
	for _, raw := range in {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" {
			continue
		}
		kind, ok := onNames[name]
		if !ok {
			return nil, fmt.Errorf("未知的 on 取值 %q（可选：%s）", raw, strings.Join(AdvanceableNames(), ", "))
		}
		out[kind] = true
	}
	if len(out) == 0 {
		return DefaultOn(), nil
	}
	return out, nil
}

// firstForbidden 返回聚合名里第一个禁用字符。
func firstForbidden(name string) (string, bool) {
	if i := strings.IndexAny(name, forbiddenNameChars); i >= 0 {
		r := []rune(name[i:])[0]
		return string(r), true
	}
	return "", false
}
