package route

import (
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func enabled(v bool) *bool { return &v }

// 正常配置：大小写保留、键小写、重复目标去重且保持首次出现顺序。
func TestNormalizeValid(t *testing.T) {
	snap, err := Normalize(Settings{
		Items: []Config{{
			Name: "Deepseek-V4.1F-Aggr",
			Targets: []ConfigTarget{
				{Realm: "global", Model: "deepseek-v4.1-flash"},
				{Realm: "global", Model: "deepseek-v4.1-flash-sg"},
				{Realm: "CN", Model: "deepseek-v4.1-flash"},              // realm 大小写归一
				{Realm: "cn", Model: "deepseek-v4.1-flash"},              // 与上一行等价 → 去重
				{Realm: "cn", Model: "deepseek-v4-flash"},
			},
		}},
	}, Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	it, ok := snap.Items["deepseek-v4.1f-aggr"]
	if !ok {
		t.Fatalf("键应小写归一，实际 keys=%v", keysOf(snap.Items))
	}
	if it.Name != "Deepseek-V4.1F-Aggr" {
		t.Fatalf("展示名应保留原始大小写，got %q", it.Name)
	}
	if len(it.Targets) != 4 {
		t.Fatalf("重复目标应去重为 4，got %d: %+v", len(it.Targets), it.Targets)
	}
	if it.Targets[2].Realm != "cn" || it.Targets[3].Model != "deepseek-v4-flash" {
		t.Fatalf("去重应保留首次出现位置，got %+v", it.Targets)
	}
}

// 非法配置必须逐条报错（面板据此定位），且不得产出可用快照。
func TestNormalizeRejects(t *testing.T) {
	hidden := map[string]struct{}{"cn:hidden-model": {}}
	known := map[string]struct{}{"glm-5.2": {}}

	tests := []struct {
		name string
		set  Settings
		want string
	}{
		{"空名", Settings{Items: []Config{{Targets: []ConfigTarget{{"cn", "m"}}}}}, "name 不能为空"},
		{"重名", Settings{Items: []Config{
			{Name: "A", Targets: []ConfigTarget{{"cn", "m"}}},
			{Name: "a", Targets: []ConfigTarget{{"cn", "n"}}},
		}}, "重复"},
		{"冒号", Settings{Items: []Config{{Name: "cn:x", Targets: []ConfigTarget{{"cn", "m"}}}}}, "不能包含"},
		{"逗号", Settings{Items: []Config{{Name: "a,b", Targets: []ConfigTarget{{"cn", "m"}}}}}, "不能包含"},
		{"等号", Settings{Items: []Config{{Name: "a=b", Targets: []ConfigTarget{{"cn", "m"}}}}}, "不能包含"},
		{"与真实模型同名", Settings{Items: []Config{{Name: "GLM-5.2", Targets: []ConfigTarget{{"cn", "m"}}}}}, "遮蔽"},
		{"空目标", Settings{Items: []Config{{Name: "A"}}}, "targets 不能为空"},
		{"目标过多", Settings{Items: []Config{{Name: "A", Targets: []ConfigTarget{
			{"cn", "a"}, {"cn", "b"}, {"cn", "c"}, {"cn", "d"},
			{"cn", "e"}, {"cn", "f"}, {"cn", "g"}, {"cn", "h"}, {"cn", "i"},
		}}}}, "目标最多"},
		{"非法 realm", Settings{Items: []Config{{Name: "A", Targets: []ConfigTarget{{"us", "m"}}}}}, "realm 必须是"},
		{"空模型", Settings{Items: []Config{{Name: "A", Targets: []ConfigTarget{{"cn", "  "}}}}}, "model 不能为空"},
		{"模型带前缀", Settings{Items: []Config{{Name: "A", Targets: []ConfigTarget{{"cn", "global:m"}}}}}, "不要带 realm 前缀"},
		{"目标指向聚合", Settings{Items: []Config{
			{Name: "B", Targets: []ConfigTarget{{"cn", "m"}}},
			{Name: "A", Targets: []ConfigTarget{{"cn", "b"}}},
		}}, "禁止递归展开"},
		{"目标被隐藏", Settings{Items: []Config{{Name: "A", Targets: []ConfigTarget{{"cn", "hidden-model"}}}}}, "黑名单"},
		{"未知 on", Settings{Items: []Config{{Name: "A", On: []string{"whatever"}, Targets: []ConfigTarget{{"cn", "m"}}}}}, "未知的 on"},
		{"预算越界", Settings{MaxAttempts: 99, Items: []Config{{Name: "A", Targets: []ConfigTarget{{"cn", "m"}}}}}, "max_attempts"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Normalize(tc.set, Options{Hidden: hidden, KnownModels: known})
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got: %v", tc.want, err)
			}
		})
	}
}

// 总开关关闭 / 未配置 → 一律不命中（零回归的入口条件）。
func TestLookupMiss(t *testing.T) {
	if _, ok := (&Snapshot{Enabled: true}).Lookup("x"); ok {
		t.Fatal("空配置不应命中")
	}
	snap, err := Normalize(Settings{
		Enabled: enabled(false),
		Items:   []Config{{Name: "A", Targets: []ConfigTarget{{"cn", "m"}}}},
	}, Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := snap.Lookup("A"); ok {
		t.Fatal("enabled=false 时不应命中")
	}

	var nilSnap *Snapshot
	if _, ok := nilSnap.Lookup("A"); ok {
		t.Fatal("nil 快照不应命中（调用方可能未注入 Routes）")
	}
}

// 客户端可能写 "cn:Aggr"/"Global:aggr"/全小写，都应命中同一条链。
func TestLookupNameInsensitive(t *testing.T) {
	snap, err := Normalize(Settings{
		Items: []Config{{Name: "Deepseek-Aggr", Targets: []ConfigTarget{{"global", "m1"}, {"cn", "m2"}}}},
	}, Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, in := range []string{"Deepseek-Aggr", "deepseek-aggr", "cn:Deepseek-Aggr", "global:deepseek-aggr", "  DEEPSEEK-AGGR  "} {
		c, ok := snap.Lookup(in)
		if !ok {
			t.Fatalf("%q 应命中", in)
		}
		if len(c.Targets) != 2 || c.Targets[0].Realm != "global" || c.Targets[1].Realm != "cn" {
			t.Fatalf("%q 目标顺序错误: %+v", in, c.Targets)
		}
	}
}

// 默认推进集：限流/5xx/容量信号/账号级故障可推进；会话失效、内容拦截、WAF、
// 请求级错误一律不可推进（否则会掩盖真实原因或放大风控）。
func TestShouldAdvanceDefault(t *testing.T) {
	snap, err := Normalize(Settings{
		Items: []Config{{Name: "A", Targets: []ConfigTarget{{"cn", "m"}, {"global", "m"}}}},
	}, Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	chain, ok := snap.Lookup("A")
	if !ok {
		t.Fatal("应命中")
	}

	advance := []upstream.ErrKind{
		upstream.ErrSoftRate, upstream.ErrServer, upstream.ErrUpstreamBusy,
		upstream.ErrNotFound, upstream.ErrHardCredit, upstream.ErrAccountFault,
		upstream.ErrModelBlocked,
	}
	for _, k := range advance {
		if !chain.ShouldAdvance(k) {
			t.Fatalf("%v 应允许推进", k)
		}
	}
	stay := []upstream.ErrKind{
		upstream.ErrSessionDead, upstream.ErrContentBlocked, upstream.ErrWafBlock,
		upstream.ErrPromptTooLong, upstream.ErrBadParams, upstream.ErrModelParamInvalid,
		upstream.ErrToolMismatch, upstream.ErrImageInvalid, upstream.ErrClient, upstream.ErrNone,
	}
	for _, k := range stay {
		if chain.ShouldAdvance(k) {
			t.Fatalf("%v 不应推进", k)
		}
	}
}

// on 显式配置只保留列出的类。
func TestShouldAdvanceCustomOn(t *testing.T) {
	snap, err := Normalize(Settings{
		Items: []Config{{
			Name: "A", On: []string{"server"},
			Targets: []ConfigTarget{{"cn", "m"}, {"global", "m"}},
		}},
	}, Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	chain, _ := snap.Lookup("A")
	if !chain.ShouldAdvance(upstream.ErrServer) {
		t.Fatal("server 应允许推进")
	}
	if chain.ShouldAdvance(upstream.ErrSoftRate) {
		t.Fatal("未列出的 soft_rate 不应推进")
	}
}

// 预算：min(2N,6)，至少 1；配置值优先。
func TestBudget(t *testing.T) {
	cases := []struct{ n, want int }{{0, 1}, {1, 2}, {2, 4}, {3, 6}, {4, 6}, {8, 6}}
	for _, c := range cases {
		if got := DefaultBudget(c.n); got != c.want {
			t.Fatalf("DefaultBudget(%d)=%d want %d", c.n, got, c.want)
		}
	}

	mk := func(max int) *Snapshot {
		snap, err := Normalize(Settings{
			MaxAttempts: max,
			Items:       []Config{{Name: "A", Targets: []ConfigTarget{{"cn", "m1"}, {"cn", "m2"}, {"cn", "m3"}}}},
		}, Options{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return &snap
	}
	if c, _ := mk(0).Lookup("A"); c.Budget != 6 {
		t.Fatalf("默认预算应为 min(2*3,6)=6，got %d", c.Budget)
	}
	if c, _ := mk(2).Lookup("A"); c.Budget != 2 {
		t.Fatalf("配置预算应优先，got %d", c.Budget)
	}
}

func keysOf(m map[string]*Item) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
