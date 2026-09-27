### English

- Trae Solo `4008` ("Your requests have exceeded the quota") is a per-window request cap, not an exhausted entitlement, so it is now treated as a rate limit: the account fails over to the next one and cools briefly, instead of being parked until local midnight with no failover. Accounts were being taken out of rotation for hours while still holding most of their credits, which is why Trae requests failed with "no trae accounts available".

### 中文

- Trae Solo 的 `4008`（"Your requests have exceeded the quota"）是**按窗口的请求次数上限**，不是额度真的用尽，因此现在按限流处理：会切换到下一个账号并短暂冷却，而不是把账号停到次日零点且不切换。此前账号在仍持有大部分额度的情况下被移出轮换数小时，导致 Trae 请求报 "no trae accounts available"。
