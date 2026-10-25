package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 品牌词两种书写形态按字节构造：整词字面量在本仓编辑/传输链路上被改写过，
// 用字节可保证测试与实现比对的是同一形态。
var (
	brandSpaced = string([]byte{0x44, 0x65, 0x65, 0x70, 0x53, 0x65, 0x65, 0x6b,
		0x20, 0x48, 0x61, 0x72, 0x6e, 0x65, 0x73, 0x73}) // DeepSeek + 空格 + Harness
	brandJoined = string([]byte{0x44, 0x65, 0x65, 0x70, 0x53, 0x65, 0x65, 0x6b,
		0x52, 0x75, 0x6e, 0x74, 0x69, 0x6d, 0x65}) // DeepSeek + Runtime
)

// brandRewriteClient 造一个假上游：记录出站请求体，回一段含指定短语的 SSE。
func brandRewriteClient(t *testing.T, replyContent string) (*upstream.Client, func() string) {
	t.Helper()
	var mu sync.Mutex
	var sent string
	quoted, err := json.Marshal(replyContent)
	if err != nil {
		t.Fatalf("marshal reply: %v", err)
	}
	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			mu.Lock()
			sent = string(raw)
			mu.Unlock()
			body := `data: {"choices":[{"delta":{"content":` + string(quoted) + `}}]}` + "\n\n" +
				`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":3}}` + "\n\n" +
				"data: [DONE]\n\n"
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	client.SanitizeFingerprints.Store(true) // 与生产默认一致：净化层开启
	return client, func() string {
		mu.Lock()
		defer mu.Unlock()
		return sent
	}
}

// 品牌词映射只作用于**出站请求**：上游回复里出现同一短语时必须原样透传给客户端。
// 净化管线只接在 prepareBody（出站组装）上，响应路径（SSE 透传 / 非流式聚合）
// 不经过任何 sanitize* 函数——这条测试把这个边界钉死。
func TestBrandRewriteOutboundOnly(t *testing.T) {
	phrase := "Current " + brandSpaced
	reply := "回答里出现了 " + phrase + " 这个词"

	for _, tc := range []struct {
		name    string
		stream  bool
		reqBody string
	}{
		{
			name:    "非流式聚合",
			reqBody: `{"model":"m1","stream":false,"messages":[{"role":"system","content":"` + phrase + `"},{"role":"user","content":"hi"}]}`,
		},
		{
			name:    "流式透传",
			reqBody: `{"model":"m1","stream":true,"messages":[{"role":"system","content":"` + phrase + `"},{"role":"user","content":"hi"}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up, sentBody := brandRewriteClient(t, reply)
			p := testPoolWith(&auth.Auth{UID: "cn1", AccessToken: "at1", ExpiresAt: 9999999999})
			h := NewHandler(Config{Pool: p, Upstream: up, Live: livecfg.New(livecfg.Snapshot{})})

			rec := postChat(h, tc.reqBody, nil)
			if rec.Code != 200 {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}

			// ① 出站请求：品牌词已按上游语义改写
			sent := sentBody()
			if strings.Contains(sent, brandSpaced) {
				t.Errorf("出站请求仍含带空格形态（应改写）: %s", sent)
			}
			if !strings.Contains(sent, brandJoined) {
				t.Errorf("出站请求未被改写: %s", sent)
			}

			// ② 客户端收到的回复：原样（改写不得触碰响应）
			if !strings.Contains(rec.Body.String(), phrase) {
				t.Errorf("响应被改写了，映射不应触及响应: %s", rec.Body.String())
			}
		})
	}
}
