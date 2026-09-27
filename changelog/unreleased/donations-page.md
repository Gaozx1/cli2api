### English

- Add a public `/donations` page where anyone can contribute a WorkBuddy, Qoder, Trae, Devin, or Command Code account and receive New API credit for it. The account is validated and imported into the pool first, and the credit is issued only afterwards, so an unusable credential is never rewarded.
- Add `GET /api/donations` (accepted formats) and `POST /api/donations` (submit a contribution). Both are public by design: a contributor has no console key, and the reward goes to their own numeric New API user id.
- Configure the donation site under System settings with `donation_base_url` and `donation_token`. The token is stored as a secret and is never returned by the settings API.

### 中文

- 新增公开的 `/donations` 贡献页面：任何人都可以贡献 WorkBuddy、Qoder、Trae、Devin 或 Command Code 账号并获得 New API 额度。账号先经过校验并导入账号池，之后才发放额度，因此不可用的凭据不会被奖励。
- 新增 `GET /api/donations`（可贡献类型）与 `POST /api/donations`（提交贡献）。两者按设计均为公开接口：贡献者没有控制台密钥，额度发放到其本人的 New API 数字用户 ID。
- 在「系统设置」里用 `donation_base_url` 和 `donation_token` 配置贡献站点。令牌以密钥形式保存，系统设置接口不会返回它。
