### English

- Add a public `/donations` page where anyone can contribute a WorkBuddy, Qoder, Trae, Devin, or Command Code account and receive New API credit for it.
- **Every region is contributable.** Qoder ships global+cn and WorkBuddy ships cn+global, and each is listed separately, so a contributor can pick the region their account actually belongs to instead of only the provider default.
- **Web authorization**: `POST /api/donations` creates the account, opens the provider login, and returns the URL to visit. `GET /api/donations/sessions/{id}` polls it and settles once the login completes; `DELETE` abandons the round.
- **Callback authorization**: providers whose login cannot complete through a loopback redirect (Trae, Devin, Codex) are reported with `callback_required`, and the contributor finishes by pasting the callback URL to `POST /api/donations/sessions/{id}/callback`. Capabilities come from the provider adapters, so a new provider needs no UI change.
- **The reward is server policy.** `credit_usd` is accepted for compatibility and ignored, so a public request can never price its own payout.
- **A contributed credential cannot choose its upstream host.** `base_url` / `baseUrl` / `api_host` / `apiHost` are stripped from a pasted payload before the importer sees it, at the top level and nested, because providers prefer the credential value over their constant and the pool routes other people's requests through the account.
- A pasted credential imports disabled: it has only passed a shape check, so enabling it is an operator decision. The contributor is still credited. A web-authorized contribution is created enabled, because a Qoder login needs the account's worker running, and an account with no usable credential never reports ready, so it is never routed to.
- The reward is issued exactly once even when several polls observe the same completed login, and a finished round cannot be settled again.
- Session ids come from `crypto/rand`: an id is what separates an anonymous caller from somebody else's in-flight round.
- Pending rounds are persisted and reclaimed by a background sweep after 15 minutes, so an abandoned contribution cannot leave its placeholder account behind — including across a restart, and even when no further request arrives. Finished rounds expire after 24 hours.
- `POST /api/donations/sessions/{id}/restart` re-opens a pending round whose provider handshake was lost (for example across a restart) instead of stranding it, reusing the same account.
- Providers without a browser login keep the pasted-credential path at `POST /api/donations/credential`.
- Configure the donation site under System settings with `donation_base_url` and `donation_token`. The token is stored as a secret and is never returned by the settings API.

### 中文

- 新增公开的 `/donations` 贡献页面：任何人都可以贡献 WorkBuddy、Qoder、Trae、Devin 或 Command Code 账号并获得 New API 额度。
- **每个区域都可以贡献。** Qoder 有 global+cn，WorkBuddy 有 cn+global，二者分别列出，贡献者可以选择账号真正所属的区域，而不是只能选平台默认区域。
- **网页授权**：`POST /api/donations` 创建账号并返回需要打开的登录地址；`GET /api/donations/sessions/{id}` 轮询进度，登录完成后发放额度；`DELETE` 放弃本次贡献。
- **回调授权**：登录无法通过本地回调完成的平台（Trae、Devin、Codex）会返回 `callback_required`，贡献者把回调地址粘贴到 `POST /api/donations/sessions/{id}/callback` 完成授权。这些能力来自平台适配器，新增平台无需改动界面。
- **奖励金额由服务端决定。** `credit_usd` 仅为兼容而接受、实际忽略，公开请求无法自行决定发放金额。
- **贡献的凭据不能自选上游主机。** 粘贴的凭据在进入导入器之前会剥离 `base_url` / `baseUrl` / `api_host` / `apiHost`（含嵌套），因为平台会优先使用凭据里的值，而账号池会用该账号承载他人的请求。
- 粘贴凭据导入的账号为禁用状态：它只通过了形状检查，启用与否应由运营者决定；奖励照发。网页授权贡献的账号以启用状态创建，因为 Qoder 的登录需要账号的 worker 运行；没有可用凭据的账号不会变为就绪，因此不会被调度。
- 即使多个轮询同时观察到同一次登录完成，奖励也只发放一次；已结束的贡献不会被重复结算。
- 会话 id 来自 `crypto/rand`：它是匿名调用者与他人进行中流程之间的唯一屏障。
- 待处理的贡献会持久化，并由后台扫描在 15 分钟后回收，因此被放弃的贡献不会留下占位账号 —— 包括重启之后，也包括之后再也没有请求到达的情况。已结束的会话在 24 小时后过期。
- `POST /api/donations/sessions/{id}/restart` 可在平台握手丢失（例如重启后）重新发起授权，而不是让流程卡死，并复用同一个账号。
- 不支持网页授权的平台保留 `POST /api/donations/credential` 的粘贴凭据方式。
- 在「系统设置」里用 `donation_base_url` 和 `donation_token` 配置贡献站点。令牌以密钥形式保存，系统设置接口不会返回它。
