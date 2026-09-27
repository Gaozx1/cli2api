### English

- A contributed account is now verified against its provider before any reward is paid, so a credential that only passes the local shape check is no longer rewarded. A provider that rejects the credential is reported as such; a provider that is merely unreachable right now is reported as retryable, so a valid account is never refused over a transient upstream fault.
- One credential can be rewarded only once: a re-submission of the same account is refused, including when it is re-sent under a different name or with a `base_url` override. There is no per-user cap, so a contributor holding several genuine accounts is paid for each of them.
- The reward ledger is persisted, so a restart does not re-open an already-paid claim.

### 中文

- 贡献的账号在发放额度之前会向平台真实验证，因此仅通过本地形状检查的凭据不再能获得奖励。平台明确拒绝该凭据时会如实报告；平台只是暂时不可达时报告为可重试，因此不会因为上游偶发故障而拒绝有效账号。
- 同一个凭据只能获得一次奖励：重复提交会被拒绝，包括改名后重提、或附加 `base_url` 覆盖的情况。不设每人上限，因此持有多个真实账号的贡献者每个账号都能获得奖励。
- 奖励台账会持久化，重启后不会重新打开已支付过的领取。
