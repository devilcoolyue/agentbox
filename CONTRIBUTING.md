# 参与贡献

欢迎报告问题、改进文档和提交聚焦单一目的的 PR。较大的架构调整请先开 Issue 说明问题、兼容性和验证方案。项目面向个人自托管和可信团队，保留 Go 单二进制、SQLite、本机 Docker 与原生 ES Modules。

## 开发

环境与目录说明见 [开发指南](docs/development.md)、[AGENTS.md](AGENTS.md) 和 [架构计划](docs/architecture/opensource-refactor.md)。

```bash
npm ci
npm run check
npm run build
go build ./...
go test ./...
go vet ./...
```

修改 TypeScript 时一起提交 `internal/web/static/js/` 的编译产物。并发改动运行相关包的 `go test -race`；容器路径、文件安全和备份改动按 AGENTS.md 运行相应 Linux 测试。CLI 真实模型调用会使用凭证并产生费用，默认测试不执行这类调用。

## 提交与评审

- 从当前主分支创建主题分支；提交可独立理解、验证和回退的改动。
- PR 描述具体问题、修改后的行为、执行过的验证和兼容限制；未执行的验证明确标注。
- 用户可见行为、API、配置或部署变化须同步更新文档和 `CHANGELOG.md`。
- 使用合成数据测试。不要提交配置、令牌、账号目录、数据库、聊天记录或生产地址；日志和截图先脱敏。
- 不要把与任务无关的格式化、依赖升级或生成文件变化混入补丁。
- 保留第三方版权与许可，新增内置依赖时更新 `third_party/` 的来源、版本、许可及哈希。

提交贡献前，请确认你有权提供相应代码，并同意按仓库根目录 LICENSE 发布贡献。不要求额外 CLA。安全问题请按 [SECURITY.md](SECURITY.md) 私下反馈，勿开公开 Issue。
