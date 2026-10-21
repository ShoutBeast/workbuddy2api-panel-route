package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/route"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// routeCall 一次上游调用的观测值。
type routeCall struct {
	Model  string
	Stream bool
}

// newRouteUpstream 假上游：记录每次调用请求体里的 model/stream，并按 model 决定响应。
func newRouteUpstream(t *testing.T, respond func(model string) (status int, body string, stream bool)) (*upstream.Client, func() []routeCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []routeCall
	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			var peek struct {
				Model  string `json:"model"`
				Stream bool   `json:"stream"`
			}
			_ = json.Unmarshal(raw, &peek)
			mu.Lock()
			calls = append(calls, routeCall{Model: peek.Model, Stream: peek.Stream})
			mu.Unlock()

			status, body, isStream := respond(peek.Model)
			ct := "application/json"
			if isStream {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	return client, func() []routeCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]routeCall(nil), calls...)
	}
}

// newRouteHandler 构造一个挂了聚合配置的 handler（单 cn 账号，realm 由 Domain 缺省为 cn）。
func newRouteHandler(t *testing.T, up *upstream.Client, items []route.Config) *Handler {
	t.Helper()
	return newRouteHandlerWithPool(t, up,
		testPoolWith(&auth.Auth{UID: "cn1", AccessToken: "at1", ExpiresAt: 9999999999}), items)
}

// newRouteHandlerWithPool 同上，但由调用方提供账号池（用于构造「全池锁定」等场景）。
func newRouteHandlerWithPool(t *testing.T, up *upstream.Client, p *pool.Pool, items []route.Config) *Handler {
	t.Helper()
	snap, err := route.Normalize(route.Settings{Items: items}, route.Options{})
	if err != nil {
		t.Fatalf("route.Normalize: %v", err)
	}
	return NewHandler(Config{
		Pool:     p,
		Upstream: up,
		Live:     livecfg.New(livecfg.Snapshot{Routes: &snap}),
	})
}

func postChat(h *Handler, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req, _ := http.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(body)))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// 未配置聚合：请求原样透传，上游只被调用一次，model 不被改写（零回归入口条件）。
func TestRouteDispatchPassthrough(t *testing.T) {
	up, calls := newRouteUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := newRouteHandler(t, up, nil) // 空配置 → 无聚合

	rec := postChat(h, `{"model":"glm-5.2","stream":true,"messages":[]}`, nil)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := calls()
	if len(got) != 1 || got[0].Model != "glm-5.2" {
		t.Fatalf("上游调用=%+v want 单次 glm-5.2", got)
	}
}

// 目标 1 模型级限流（6004）→ 推进到目标 2 成功；客户端只看到一次成功响应，
// 且响应 model 回显为聚合名。
func TestRouteDispatchAdvancesOnRateLimit(t *testing.T) {
	up, calls := newRouteUpstream(t, func(model string) (int, string, bool) {
		if strings.Contains(model, "m1") {
			return 429, `{"code":6004,"msg":"将在 2026-10-21 20:00:00 UTC+8 重置"}`, false
		}
		return 200, sseOK, true
	})
	h := newRouteHandler(t, up, []route.Config{{
		Name:    "Deepseek-Aggr",
		Targets: []route.ConfigTarget{{Realm: "cn", Model: "m1"}, {Realm: "cn", Model: "m2"}},
	}})

	rec := postChat(h, `{"model":"Deepseek-Aggr","stream":true,"messages":[]}`, nil)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := calls()
	if len(got) != 2 {
		t.Fatalf("上游调用=%+v want 2 次（m1 限流后推进 m2）", got)
	}
	// 出站前 realm 前缀会被剥掉（网关侧路由协议），上游只看到裸模型名。
	if got[0].Model != "m1" || got[1].Model != "m2" {
		t.Fatalf("目标顺序错误: %+v", got)
	}
	if !strings.Contains(rec.Body.String(), "Deepseek-Aggr") {
		t.Fatalf("响应应回显聚合名，body=%s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "6004") {
		t.Fatalf("不应把目标 1 的限流错误透给客户端: %s", rec.Body.String())
	}
}

// 请求级错误（400 参数）不推进：换目标只会掩盖真实原因。
func TestRouteDispatchStopsOnRequestError(t *testing.T) {
	up, calls := newRouteUpstream(t, func(string) (int, string, bool) {
		return 400, `{"code":11101,"msg":"Unmarshal chat params failed"}`, false
	})
	h := newRouteHandler(t, up, []route.Config{{
		Name:    "Deepseek-Aggr",
		Targets: []route.ConfigTarget{{Realm: "cn", Model: "m1"}, {Realm: "cn", Model: "m2"}},
	}})

	rec := postChat(h, `{"model":"Deepseek-Aggr","stream":true,"messages":[]}`, nil)
	if rec.Code != 400 {
		t.Fatalf("status=%d want 400（透传目标错误）body=%s", rec.Code, rec.Body.String())
	}
	if got := calls(); len(got) != 1 {
		t.Fatalf("请求级错误不应推进，实际调用=%+v", got)
	}
}

// 全部目标限流：调用次数受预算约束（min(2N,6)），最后一个目标的错误透出。
func TestRouteDispatchBudgetAndExhaustion(t *testing.T) {
	up, calls := newRouteUpstream(t, func(string) (int, string, bool) {
		return 429, `{"code":6004,"msg":"quota"}`, false
	})
	h := newRouteHandler(t, up, []route.Config{{
		Name: "Deepseek-Aggr",
		Targets: []route.ConfigTarget{
			{Realm: "cn", Model: "m1"}, {Realm: "cn", Model: "m2"}, {Realm: "cn", Model: "m3"},
		},
	}})

	rec := postChat(h, `{"model":"Deepseek-Aggr","stream":true,"messages":[]}`, nil)
	if rec.Code != 429 {
		t.Fatalf("status=%d want 429 body=%s", rec.Code, rec.Body.String())
	}
	got := calls()
	if len(got) > route.DefaultBudget(3) {
		t.Fatalf("调用次数 %d 超预算 %d", len(got), route.DefaultBudget(3))
	}
	if len(got) < 3 {
		t.Fatalf("三个目标都应被尝试，实际 %d 次", len(got))
	}
}

// 流式：目标 1 只发出注释心跳（空流 → handler 报错）时，网关尚未向客户端写出
// 真实内容，仍应推进到目标 2 —— 这正是「注释不算提交」的语义。
func TestRouteDispatchAdvancesAfterCommentOnlyStream(t *testing.T) {
	up, calls := newRouteUpstream(t, func(model string) (int, string, bool) {
		if strings.Contains(model, "m1") {
			return 200, ": keepalive\n\n", true
		}
		return 200, sseOK, true
	})
	h := newRouteHandler(t, up, []route.Config{{
		Name:    "Deepseek-Aggr",
		Targets: []route.ConfigTarget{{Realm: "cn", Model: "m1"}, {Realm: "cn", Model: "m2"}},
	}})

	rec := postChat(h, `{"model":"Deepseek-Aggr","stream":true,"messages":[]}`, nil)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := calls(); len(got) != 2 {
		t.Fatalf("注释心跳后应推进，实际调用=%+v", got)
	}
	if !strings.Contains(rec.Body.String(), "你好") {
		t.Fatalf("客户端应收到目标 2 的正文，body=%s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "keepalive") {
		t.Fatalf("目标 1 的心跳不应泄漏给客户端: %s", rec.Body.String())
	}
}

// 客户端伪造内部回显头时，非聚合请求不得被篡改 model。
func TestRouteDispatchIgnoresSpoofedEchoHeader(t *testing.T) {
	up, _ := newRouteUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := newRouteHandler(t, up, nil)

	rec := postChat(h, `{"model":"glm-5.2","stream":true,"messages":[]}`,
		map[string]string{routeEchoHeader: "Spoofed-Name"})
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "Spoofed-Name") {
		t.Fatalf("伪造的回显头不应生效: %s", rec.Body.String())
	}
}

// 第一个目标已被全池锁定（模型级冷却）→ 本地预检直接跳过：零上游调用、零等待，
// 直接命中第二个目标。这条防的是「上游 handler 对 14003 全池挡住先等 10s 再报」，
// 多目标链上会串成 N×10s 的干耗。
func TestRouteDispatchSkipsLocallyBlockedTarget(t *testing.T) {
	up, calls := newRouteUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "cn1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.CooldownModelBusy("cn1", "m1", "14003 model busy") // m1 在 cn 域全池锁定

	h := newRouteHandlerWithPool(t, up, p, []route.Config{{
		Name:    "Aggr",
		Targets: []route.ConfigTarget{{Realm: "cn", Model: "m1"}, {Realm: "cn", Model: "m2"}},
	}})

	rec := postChat(h, `{"model":"Aggr","stream":true,"messages":[]}`, nil)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := calls()
	if len(got) != 1 || got[0].Model != "m2" {
		t.Fatalf("被锁定的目标不应进 handler，实际调用=%+v", got)
	}
	if !strings.Contains(rec.Body.String(), "你好") {
		t.Fatalf("应收到第二跳正文: %s", rec.Body.String())
	}
}

// 全部目标都在本地被锁定 → 零上游调用，按限流语义回 429 + Retry-After。
func TestRouteDispatchAllBlockedReturns429(t *testing.T) {
	up, calls := newRouteUpstream(t, func(string) (int, string, bool) {
		t.Fatal("全部目标已锁定，不应有任何上游调用")
		return 500, "", false
	})
	p := testPoolWith(&auth.Auth{UID: "cn1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.CooldownModelBusy("cn1", "m1", "14003 model busy")
	p.CooldownModelBusy("cn1", "m2", "14003 model busy")

	h := newRouteHandlerWithPool(t, up, p, []route.Config{{
		Name:    "Aggr",
		Targets: []route.ConfigTarget{{Realm: "cn", Model: "m1"}, {Realm: "cn", Model: "m2"}},
	}})

	rec := postChat(h, `{"model":"Aggr","stream":true,"messages":[]}`, nil)
	if rec.Code != 429 {
		t.Fatalf("status=%d want 429 body=%s", rec.Code, rec.Body.String())
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Fatal("429 应带 Retry-After")
	}
	if n := len(calls()); n != 0 {
		t.Fatalf("不应有上游调用，实际 %d 次", n)
	}
	if !strings.Contains(rec.Body.String(), "route_exhausted") {
		t.Fatalf("错误体应说明聚合链耗尽: %s", rec.Body.String())
	}
}
