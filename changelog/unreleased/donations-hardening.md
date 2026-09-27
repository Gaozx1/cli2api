### English

- Donation rewards are capped at $50 per contribution. A `credit_usd` above the cap is refused before any account is imported or created, because the endpoint is public and the reward is paid from the operator's own New API quota.
- A contributed credential can no longer choose the upstream host: `base_url` / `api_host` are stripped from the submitted payload, recursively, before the provider importer sees it.
- A pasted credential no longer enables its account. Accounts contributed through `POST /api/donations/credential` wait for an operator, because nothing about a submitted credential is validated against the provider and an enabled account carries other people's requests. The browser-authorization flow still enables the account once the provider login completes.
- Concurrent polls of one authorization round can no longer credit twice: the settle path is claimed under the lock instead of inferred from a pre-lock read.
- Settled rounds are dropped from memory after 24 hours, and expired placeholders are deleted by the caller rather than a detached goroutine, so an unauthenticated caller cannot grow the session map without bound.
- Session ids are drawn from `crypto/rand` instead of the clock.

### 中文

- 捐赠奖励单次上限为 50 美元。`credit_usd` 超限时，会在导入或创建任何账号之前直接拒绝——该接口是公开的，奖励出自运营者自己的 New API 额度。
- 贡献的凭据不再能指定上游主机：提交载荷中的 `base_url` / `api_host`（含嵌套）会在进入 provider importer 之前被剥离。
- 粘贴凭据不再自动启用账号。通过 `POST /api/donations/credential` 贡献的账号改为等待运营者启用——提交的凭据从未对 provider 做过验证，而启用后的账号会承载他人的请求。浏览器授权流程在 provider 登录完成后仍会启用账号。
- 同一轮授权的并发轮询不再可能重复发放奖励：结算路径改为在锁内抢占，而不再依赖加锁前的读取结果判断。
- 已结束的会话在 24 小时后从内存回收；超时的占位账号改由调用方删除，而不是丢给游离的 goroutine，避免未鉴权的调用者让会话表无限增长。
- 会话 id 改用 `crypto/rand` 生成，不再取自时钟。
