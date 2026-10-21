# 版本发布

不跟上游 `1.x`。发版是打时间标签并推送，工作流看到 `v*` 才创建 Release。

版本号是上海时间，形如 `v2026.9.26.711`：

- `2026.9.26` 是日期，月和日不补零
- `711` 是 7:11，时不补零、分两位、不带秒
- 标签、Release、二进制用同一个号；二进制里不带 `v`，面板会自己加
- 不要打 `v1.x` 标签，也不要手改 `appVersion`

```bash
# 注意：本机（Windows / git-bash）没有 tzdata，`TZ=Asia/Shanghai date` 会**静默回落 UTC**，
# 打出来的标签整整早 8 小时。用显式加 8 小时，兼容有无 tzdata 两种环境：
VERSION=$(TZ=Asia/Shanghai date +%Y.%-m.%-d.%-H%M)
[ "$(TZ=UTC date +%H)" = "$(TZ=Asia/Shanghai date +%H)" ] && VERSION=$(date -u -d '+8 hours' +%Y.%-m.%-d.%-H%M)
git tag "v$VERSION"
git push origin "v$VERSION"
```

本地构建不注入，面板显示 `dev`。同一分钟再发才会撞号。

---

# 新功能编写规则（为了以后能顺利接收上游）

本仓库是二开分支，上游随时会有新功能要并进来。**同步成本 = 我们的改动与上游改动的重叠面积**，所以写新功能时一律按下面的顺序选落点：

## 落点优先级

1. **新包 > 新文件 > 改上游文件**。功能主体放进新目录（如 `internal/route/`），上游文件里只留**一两行接线**。
2. 新增文件永不冲突；改上游函数体是冲突之源。能加接口/回调/配置项，就不要改函数内部逻辑。
3. 需要扩展上游结构体时，优先用**我方独立结构 + 组合**，不要往上游 struct 里塞字段（字段顺序、tag 都是上游常改的地方）。

## 标记我方改动

无法避免改上游文件时，改动块用成对注释包起来，方便同步时一眼定位、只解自己这块：

```go
// route: begin —— 我方扩展（同步上游时优先保留本块）
...新增的接线代码...
// route: end
```

同步上游时先 `grep -rn "route: begin"`，冲突只需在这几处逐块确认。

## 禁止事项（会放大同步成本）

- 全仓 `gofmt` / 格式化上游文件、调整 import 顺序、重排 struct 字段
- 重命名上游的标识符、函数、包、配置键
- 删除或改写上游的注释、`README`、`.github/workflows/`、`AutoBot.yml` 等既有内容
- 例外：本文件（`AGENTS.md`）末尾的「新功能编写规则」一节属**追加**，同步上游时整块保留即可；若上游也改到该文件，冲突属已知成本，按「上游内容在上、我方章节在下」合并
- 把上游「顺手优化」一起做掉（那是上游的活，合并时会对撞）

## 提交粒度

- 一个功能一个 commit，信息带前缀：`feat(route): <一句话>`
- 不把「功能 + 格式化 + 无关修复」混在一个 commit 里，否则 rebase 时无法逐个重放
- 依赖上游内部实现的改动，在 commit body 里写明依赖了哪个上游文件/函数，上游重构时可据此快速定位

## 同步上游后自检

```bash
git log --oneline upstream/main..HEAD      # 我方补丁清单
git diff --stat upstream/main..HEAD        # 我方净差异（越短越好）
grep -rn "route: begin" --include=*.go      # 我方接线点
go build ./... && go vet ./...
```

净差异里出现大量上游文件、或出现我们没标记过的改动，说明有功能写错了落点，应在下次同步前重构掉。

---

# 本地开发调试

## 不要占用 7863

`7863` 是正式部署 / 示例配置的默认端口，本机可能已有实例、隧道或反向代理占着它。

**本地开发、调试、冒烟测试一律换端口**，用本机其它空闲端口（如 `17863`、`27863`）。

```powershell
# 启动前先确认端口空闲（无输出 = 可用）
Get-NetTCPConnection -LocalPort 7863 -ErrorAction SilentlyContinue

# 开发实例：改 listen，不要动正式配置
# dev 配置里的 "listen" 写成 "127.0.0.1:17863"
go run ./cmd/server -config .\config.json
```

约定：

- 开发配置的 `listen` 用 `127.0.0.1:<空闲端口>`，不要写 `:17863`（那样会绑全网卡）
- 冒烟 / 探活 / curl 一律打开发端口，示例：`Invoke-WebRequest http://127.0.0.1:17863/healthz`
- 开发配置复用仓库已忽略的 `config.json`（`.gitignore` 已覆盖），或放在**仓库外**；不要新建会被提交的 `dev config`
- 端口被占就再换一个，别去 kill 别人的进程
- **结束调试实例只能按 PID**：启动时把 `$p.Id` 记下来，收尾 `Stop-Process -Id $p.Id`。
  禁止 `Stop-Process -Name wb2api -Force` / `Get-Process wb2api | Stop-Process` 这类**按进程名**的杀法——
  本机同时跑着正式实例（7863），按名杀会把线上一起打挂（已发生过一次）
- 收尾时确认开发实例已退出（`Get-NetTCPConnection -LocalPort 17863`），避免下次启动报端口占用
- **但涉及 UI 改动时例外：调试实例与调试浏览器页面都要留着**，交用户亲自验收后再关。
  关掉页面等于把验收现场拆了（用户已明确要求）。此时应在总结里写清：调试端口、api_key、
  已注入的测试数据，以及"用完自己关"的提示
