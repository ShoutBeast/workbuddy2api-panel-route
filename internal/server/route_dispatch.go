// route_dispatch.go 模型路由聚合的请求调度层（我方扩展，见 internal/route）。
//
// 为什么单独一层而不是改 chatCompletions 里的轮转循环：
//   - 那个循环 400 行、十余处 return 直接写响应，改成「按目标推进」的状态机会把
//     上游文件改得面目全非（违背仓库 AGENTS.md 的“新包优先、最小接线”）；
//   - 用「惰性提交」在 mux 之前重放请求，语义等价且对四种入站协议一视同仁：
//     一次尝试只要还没向客户端写出真实内容，就可以整段丢弃换下一个目标。
//
// 与既有 upstream.HeadVetoError 的关系：那条路径在 handler 内部已把「零字节否决」
// 处理掉了（换账号）；本层处理的是「整个目标耗尽后」的推进，两者不重叠。
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// routeEchoHeader 内部头：把对外聚合名带进内层 handler，用于响应 model 回显。
// 由本层**覆盖**写入（客户端伪造的值会被覆盖或删除），不参与上游出站。
const routeEchoHeader = "X-WB2A-Route-Echo"

// routeModelEntries 把聚合名作为合成条目加进 /v1/models（否则客户端发现不了）。
// 排序固定，避免 map 遍历顺序导致列表抖动；hidden 命中时与真实模型同口径隐藏。
func (h *Handler) routeModelEntries() []map[string]any {
	snap := h.loadLive().Routes
	if snap == nil || !snap.Enabled || len(snap.Items) == 0 {
		return nil
	}
	names := make([]string, 0, len(snap.Items))
	for _, it := range snap.Items {
		names = append(names, it.Name)
	}
	sort.Strings(names)

	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		if h.hiddenModel("cn", name) && h.hiddenModel("global", name) {
			continue
		}
		out = append(out, map[string]any{
			"id":       name,
			"object":   "model",
			"created":  1753600000,
			"owned_by": "route",
		})
	}
	return out
}

// serveWithRoutes 是有聚合命中时的调度入口；未命中一律原样交给 mux（零回归）。
func (h *Handler) serveWithRoutes(w http.ResponseWriter, r *http.Request) {
	snap := h.loadLive().Routes
	if snap == nil || r.Method != http.MethodPost || !isChatEntry(r.URL.Path) {
		h.mux.ServeHTTP(w, r)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		// 读体失败：交回原逻辑，让它按既有口径报 400/500。
		h.mux.ServeHTTP(w, r)
		return
	}
	_ = r.Body.Close()

	var peek struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if json.Unmarshal(body, &peek) != nil || peek.Model == "" {
		h.replayBody(w, r, body)
		return
	}
	// 聚合名不属于任何 realm：先剥前缀再查，客户端写 "cn:Aggr" 也能命中。
	_, bare := resolveModel(peek.Model)
	chain, hit := snap.Lookup(bare)
	if !hit {
		h.replayBody(w, r, body)
		return
	}

	var last *lazyResponse
	lastKind := upstream.ErrNone
	// skipped/earliest：本地预检跳过的目标数与最早解封时刻（仅用于最终错误与 Retry-After）
	skipped := 0
	var earliest time.Time
	skipKind := upstream.ErrNone

	for _, tgt := range chain.Targets {
		if chain.Budget <= 0 {
			break
		}
		// 本地预检：该 (realm, 模型) 已被全池锁定 → 零上游调用、零等待直接推进。
		// 为什么必须放在 handler 之前：上游合并后 handler 会对「14003 全池挡住」
		// 先等一次（busyWaitCap=10s）再报 503；多目标链上会串成 N×10s 的干耗，
		// 而这种情况本地就能判定，根本没有必要进 handler。
		if h.cfg.Pool != nil && tgt.Model != "" {
			if blk := h.cfg.Pool.ModelBlockedForRealm(tgt.Model, tgt.Realm); blk.Blocked {
				skipped++
				if !blk.Until.IsZero() && (earliest.IsZero() || blk.Until.Before(earliest)) {
					earliest = blk.Until
				}
				if upstream.IsModelBusy(blk.Reason) || strings.Contains(blk.Reason, "14003") {
					skipKind = upstream.ErrSoftRate // 限流/繁忙：最终按 429 语义答复
				} else if skipKind == upstream.ErrNone {
					skipKind = upstream.ErrServer // 无权限/无此模型等硬失败：最终 503
				}
				continue
			}
		}
		chain.Budget-- // 只在实际出站前扣减；本地无候选的跳过不耗预算

		// 每个目标都从**原始 body** 重写：既有别名改写有 cur == peek.Model 守卫，
		// 对已改写的 body 二次改写会静默失效。
		nb, ok := setBodyModel(body, tgt.Realm+":"+tgt.Model)
		if !ok {
			break
		}

		lr := newLazyResponse(w, peek.Stream)
		req := r.Clone(r.Context())
		req.Body = io.NopCloser(bytes.NewReader(nb))
		req.ContentLength = int64(len(nb))
		req.Header.Set(routeEchoHeader, peek.Model)

		h.mux.ServeHTTP(lr, req)
		lr.finish()

		if lr.committed {
			// 已向客户端写出真实内容（或非流式 2xx）：不能再换目标。
			return
		}
		last = lr

		// 目标耗尽：只有限流类错误才推进（内容拦截/会话失效/WAF 等一律透出）。
		// code==0 表示 handler 只用了 Write（SSE 常见）而没显式 WriteHeader——
		// 未提交即等价于「还没给客户端定状态码」，按 200 + 按帧内容归类。
		effCode := lr.code
		if effCode == 0 {
			effCode = http.StatusOK
		}
		kind := upstream.Classify(effCode, string(lr.buf.Bytes()))
		if effCode == http.StatusOK {
			// 上游 200 但没产出正文（空流/仅注释/仅 error 帧）：状态码不可用作判据，
			// 按帧内内容归类——限流码→可推进的限流类，其余视为上游故障。
			kind = streamFailureKind(lr.buf.Bytes())
		}
		lastKind = kind
		if !chain.ShouldAdvance(kind) {
			break
		}
	}
	if last == nil {
		// 一个目标都没真正出站：要么全部在本地被预检跳过，要么链为空。
		if skipped > 0 {
			if skipKind == upstream.ErrSoftRate {
				writeRouteExhaustedAt(w, earliest)
			} else {
				writeOpenAIError(w, http.StatusServiceUnavailable, "route_unavailable",
					"all route targets are unavailable (model blocked on every account)")
			}
		}
		return
	}
	if lastKind == upstream.ErrSoftRate {
		// 整条链都是限流：按 429 语义返回，并给出 Retry-After（不泄漏内部目标名）。
		writeRouteExhausted(w)
		return
	}
	last.commit()
}

// writeRouteExhausted 整条链都因限流失败时的响应：429 + Retry-After。
// 错误体只说“聚合链已耗尽”，每个目标的具体失败原因留在日志/面板。
func writeRouteExhausted(w http.ResponseWriter) { writeRouteExhaustedAt(w, time.Time{}) }

// writeRouteExhaustedAt 同 writeRouteExhausted，但 Retry-After 尽量取**最早解封时刻**
// （上游给了权威恢复时间时，让客户端等对时间，而不是固定 60s）。
func writeRouteExhaustedAt(w http.ResponseWriter, until time.Time) {
	secs := 60
	if !until.IsZero() {
		if d := int(time.Until(until).Seconds() + 0.999); d > 0 && d < 3600 {
			secs = d
		}
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeOpenAIError(w, http.StatusTooManyRequests, "route_exhausted",
		"all route targets are rate-limited, retry later")
}

// streamFailureKind 归类「HTTP 200 但没有正文产出」的流式失败：
//   - 帧里带 6004/14003 → 模型级限流（ErrSoftRate，可推进）；
//   - 其余（空流、只有注释、只有 error 帧）→ 上游故障（ErrServer，可推进）。
//
// 只在这条「未提交」路径上调用：能走到这里说明客户端尚未看到任何正文。
func streamFailureKind(buf []byte) upstream.ErrKind {
	if bytes.Contains(buf, []byte("6004")) || bytes.Contains(buf, []byte("14003")) {
		return upstream.ErrSoftRate
	}
	return upstream.ErrServer
}

// replayBody 把已读出的 body 装回请求后交回 mux。
func (h *Handler) replayBody(w http.ResponseWriter, r *http.Request, body []byte) {
	// 删除客户端可能伪造的回显头，避免非聚合请求获得伪造的 model 回显。
	r.Header.Del(routeEchoHeader)
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	h.mux.ServeHTTP(w, r)
}

// setBodyModel 只替换顶层 model 字段，其余字段用 json.RawMessage 原样搬运
// （避免 map[string]any 把大整数/浮点精度改掉）。
func setBodyModel(body []byte, model string) ([]byte, bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil {
		return nil, false
	}
	raw, err := json.Marshal(model)
	if err != nil {
		return nil, false
	}
	obj["model"] = raw
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return out, true
}

// lazyResponse 惰性提交的 ResponseWriter：在「真实内容」写出之前，一律只进缓冲，
// 使一次失败尝试对客户端完全无痕（可整段丢弃换下一个目标）。
//
// 提交判据：
//   - 流式：缓冲里出现 SSE 数据行（"data:"）才算真实内容——注释/心跳行
//     （": keepalive"）不算，这正是允许「注释后仍可切换」的关键；
//   - 非流式：handler 返回时状态码 < 400 才提交。
type lazyResponse struct {
	dst       http.ResponseWriter
	hdr       http.Header
	buf       bytes.Buffer
	code      int
	stream    bool
	committed bool
}

func newLazyResponse(dst http.ResponseWriter, stream bool) *lazyResponse {
	return &lazyResponse{dst: dst, hdr: make(http.Header), stream: stream}
}

func (l *lazyResponse) Header() http.Header { return l.hdr }

func (l *lazyResponse) WriteHeader(code int) {
	if l.code == 0 {
		l.code = code
	}
}

func (l *lazyResponse) Write(p []byte) (int, error) {
	if l.committed {
		return l.dst.Write(p)
	}
	n, err := l.buf.Write(p)
	if l.stream {
		l.maybeCommitStream()
	}
	return n, err
}

// Flush 未提交时不向下游 flush：一旦 flush，响应头就发出去了，重试窗口随之关闭。
// SSE 依赖 Flush 逐帧下发，所以提交之后必须真正透传。
func (l *lazyResponse) Flush() {
	if l.committed {
		if f, ok := l.dst.(http.Flusher); ok {
			f.Flush()
		}
	}
}

// maybeCommitStream 见到第一条 SSE 数据行即视为真实内容开始。
//
// 例外：整段缓冲里只有 error 帧、没有任何 choices（典型是上游「200 + 6004 error
// 帧」的流式限流形态）时**不提交**——此时客户端还没看到任何正文，仍允许换目标。
// 判据用 payload 关键字而非 HTTP 状态，因为这类失败的状态码是 200。
func (l *lazyResponse) maybeCommitStream() {
	b := l.buf.Bytes()
	if bytes.HasPrefix(b, []byte("data:")) || bytes.Contains(b, []byte("\ndata:")) {
		if isPureErrorStream(b) {
			return
		}
		l.commit()
	}
}

// isPureErrorStream 判断缓冲里是否「只有错误帧、没有正文字段」。
func isPureErrorStream(b []byte) bool {
	return bytes.Contains(b, []byte(`"error"`)) && !bytes.Contains(b, []byte(`"choices"`))
}

func (l *lazyResponse) commit() {
	if l.committed {
		return
	}
	l.committed = true
	for k, vs := range l.hdr {
		for _, v := range vs {
			l.dst.Header().Add(k, v)
		}
	}
	code := l.code
	if code == 0 {
		code = http.StatusOK
	}
	l.dst.WriteHeader(code)
	if l.buf.Len() > 0 {
		_, _ = l.dst.Write(l.buf.Bytes())
		l.buf.Reset()
	}
	if f, ok := l.dst.(http.Flusher); ok {
		f.Flush()
	}
}

// finish 在 handler 返回后调用。
//
//   - 流式：**只有已提交才算成功**。没见到真实 data 帧就说明这一轮没有向客户端
//     产出正文（空流/仅注释/仅 error 帧），保持未提交，让上层决定推进还是透出；
//     否则「200 + 空流」会被当成成功锁死，聚合永远换不了目标。
//   - 非流式：状态码 < 400 才提交（整段 body 已缓冲，提交即原样下发）。
func (l *lazyResponse) finish() {
	if l.committed {
		return
	}
	if l.stream {
		return
	}
	code := l.code
	if code == 0 {
		code = http.StatusOK
	}
	if code < http.StatusBadRequest {
		l.commit()
	}
}
