### English

- A contribution link's `?uid=` is now cleared from the address bar on every path. Previously it was only read once, at mount, and the cleanup effect depended on `navigate` — so when the app was ALREADY showing the donations page and only the query changed (an in-app link, or a pasted URL the router handled client-side), the component stayed mounted, the effect never re-ran, and the id stayed visible in the address bar with the field left unlocked. The uid is now captured into state and the query is keyed on `location.search`, so it is honoured and stripped whether the page loaded fresh or the app navigated to it.

### 中文

- 贡献链接的 `?uid=` 现在在所有情况下都会从地址栏清除。此前它只在挂载时读取一次，且清理 effect 依赖 `navigate` —— 所以当应用**已经**停在贡献页、只有 query 变化时（应用内链接，或路由器客户端处理粘贴的 URL），组件不会重新挂载、effect 不会重跑，于是 id 留在地址栏、字段也没锁定。现在 uid 记入 state、query 以 `location.search` 为依赖，因此无论整页加载还是应用内跳转，都会生效并剥离。
