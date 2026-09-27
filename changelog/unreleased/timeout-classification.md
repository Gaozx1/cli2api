### English

- A request that times out on the HTTP client's own timeout is now recorded as the account's failure, so the account is cooled down and the request fails over instead of retrying into the same wall. Previously any timeout was treated as "the caller went away" and skipped classification entirely: a wedged Qoder worker, which never answers, therefore stayed in rotation and made every request routed to it burn the full 120-second timeout while the caller saw "canceled ... Client.Timeout exceeded". Measured in production: one such account absorbed an hour of Qoder requests at 120s each with no cooldown, no failover, and no signal. A genuine caller cancellation (the client disconnected, or the caller's own context expired) is still ignored, since there is nothing to record and no one to fail over for.

### 中文

- 因 HTTP 客户端自身超时导致的请求失败，现在会记为**该账号的故障**，于是账号会被冷却、请求会切换到其他账号，而不是反复撞同一堵墙。此前任何超时都被当作"调用方已离开"直接跳过归类：于是**卡死的 Qoder worker**（永远不返回）会一直留在轮换里，每次被选中都让调用方白等满 120 秒，最终只看到 `canceled ... Client.Timeout exceeded`。线上实测：这类账号连续一小时吞掉全部 Qoder 请求，每个 120 秒，既不冷却、也不切换、毫无信号。而**真正的调用方取消**（客户端断开、或调用方自己的 context 到期）仍然忽略——那时既没有可记录的对象，也没有可切换的对象。
