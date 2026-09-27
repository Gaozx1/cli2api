### English

- A rate-limit cooldown can no longer be stretched to hours by an upstream reset time. WorkBuddy reports the usage window's absolute reset instant as its retry hint ("will reset at <timestamp>"), and the gateway took that value at face value: a single failure (backoff level 0) parked an account for up to 15 hours while it was still serving, and it bypassed both the retry-after cap and the 6-hour backoff ceiling, because those guard the HTTP retry header rather than a provider's own hint. A provider hint is now capped like any other, so a long or stale reset costs one capped wait and the account is retried and re-judged.

### 中文

- 限流冷却不再会被上游的重置时间拉长到数小时。WorkBuddy 会把额度窗口的**绝对重置时刻**当作重试提示返回（"将在 <时间> 重置"），而网关照单全收：于是**一次失败**（退避层级 0）就把账号停了最多 15 小时，而账号当时其实还能正常服务；而且它同时绕过了重试上限和 6 小时退避上限——因为那两道闸只管 HTTP 重试头，不管 provider 自己的提示。现在 provider 的提示与其他提示一样受上限约束，因此一个过长或过期的重置时间最多只代价一次封顶等待，之后账号会被重试并重新判定。
