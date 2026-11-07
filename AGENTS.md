# AGENTS.md

## 提交与发版规则

**每次提交都必须同时打 tag。** 提交与 tag 一起推送，不要只推提交。

### 版本号

遵循语义化版本 `vX.Y.Z`，在最新 tag 的基础上递增：

| 改动类型 | 递增位 | 示例 |
|---|---|---|
| `feat:` 新功能 | minor | `v0.2.0` → `v0.3.0` |
| `fix:` / `ci:` / `docs:` / `refactor:` / `perf:` | patch | `v0.3.0` → `v0.3.1` |

### 操作顺序

```bash
git add -A
git commit -m "<type>: <描述>"
git tag vX.Y.Z
git push origin master
git push origin vX.Y.Z
```

tag 会触发 `.github/workflows/release.yml`：跑测试、交叉编译 linux/windows（amd64 + arm64）、
上传二进制与 `SHA256SUMS.txt`，并自动创建 GitHub Release。所以打 tag 等于发版，务必确认构建通过。

### 注意

- 打 tag 前先跑 `go build ./...` 和 `go test ./...`
- 一次提交对应一个 tag，不要在同一个提交上打多个 tag
- 若网络导致推送失败，先确认本地 tag 已创建，恢复后补推 `git push origin master --tags`
- macOS 平台已从构建矩阵移除，不要加回
