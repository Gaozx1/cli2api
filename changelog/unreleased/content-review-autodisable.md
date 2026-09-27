### English

- An account that the provider has effectively banned is now taken out of rotation automatically. WorkBuddy answers a provider-side content ban with code 11140 ("Content failed safety review", "request illegal") to every request regardless of that request's content, which the classifier reads as a request-level rejection: no cooldown, no failover. Such an account therefore keeps being picked and keeps failing. When the last 10 consecutive calls for a WorkBuddy account all failed with 11140, the account is disabled, and the reason is shown on its card so it is clear this is an account-level block and not a bad prompt. A single success inside the window spares the account, and fewer than 10 calls is not enough evidence. The rule reads the call history, so it survives a restart and a disabled account is never re-stamped.
- Contributed accounts now carry where they came from. The accounts page shows which New API user contributed an account and which provider/region/format was contributed, so a donated account can be told apart from an operator-created one at a glance.

### 中文

- 被平台实际封禁的账号现在会自动移出轮换。WorkBuddy 对**账号级**内容封禁会无差别地对每个请求都返回 `11140`（"Content failed safety review"、"request illegal"），而分类器按错误形状把它当作**请求级**拒绝处理：不冷却、不切换。于是这种账号会一直被选中、一直失败。现在某个 WorkBuddy 账号**最近连续 10 次调用全部**因 11140 失败时，会被自动禁用，并在卡片上显示原因，说明这是账号级封禁而不是提示词问题。窗口内只要成功一次就不会禁用；不足 10 次也不作为依据。规则读取调用历史，因此重启后依然成立，已禁用的账号不会被重复标记。
- 贡献的账号现在会记录来源。账号页会显示是哪个 New API 用户贡献的，以及贡献的是哪个平台/区域/格式，便于一眼区分贡献账号与自建账号。
