### English

- A contribution link can pin the recipient: `…/donations?uid=123` fixes the New API user id to `123`. When a link pins it, the field is not rendered at all — the page shows the id read-only with a lock, so there is no control to change it. A plain `/donations` still lets the contributor type their own id. The query string is removed from the address bar as soon as the link is opened.
- One accepted contribution now pays 0.5 USD, and the page reports that same figure. Previously the payout and the displayed amount were two separate hardcoded values, so changing one would have left the page advertising an amount that no longer matched what was credited.

### 中文

- 贡献链接可以指定收款人：`…/donations?uid=123` 会把 New API 用户 ID 固定为 `123`。当链接指定了 ID 时，**该输入框根本不渲染** —— 页面只读显示这个 ID 并带一把锁，因此没有任何可修改的控件。直接访问 `/donations` 时贡献者仍可自己填写。链接打开后地址栏上的查询参数会立即消失。
- 一次成功贡献现在发放 0.5 USD，页面显示的也是同一数值。此前发放金额与页面显示是两个各自硬编码的值，改动其一会导致页面显示的数额与实际到账不符。
