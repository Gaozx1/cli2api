### English

- Add a public `/donations` page where anyone can contribute a WorkBuddy, Qoder, Trae, Devin, or Command Code account and receive New API credit for it. Contributions use the provider's own browser authorization, so a contributor never hands over a raw credential.
- **Web authorization**: `POST /api/donations` creates the account, opens the provider login, and returns the URL to visit. `GET /api/donations/sessions/{id}` polls it and issues the reward once the login completes; `DELETE` abandons the round. The reward is never credited before the account is authorized.
- The contributed account is created disabled and is enabled only after authorization succeeds, so an unfinished round cannot carry traffic.
- If the credit call fails after a successful authorization, the account stays in the pool and the response reports `credited:false` with `credit_error`, so an operator can credit it without asking for a re-login.
- Providers without a browser login keep the pasted-credential path at `POST /api/donations/credential`.
- Configure the donation site under System settings with `donation_base_url` and `donation_token`. The token is stored as a secret and is never returned by the settings API.

### 中文

- 新增公开的 `/donations` 贡献页面：任何人都可以贡献 WorkBuddy、Qoder、Trae、Devin 或 Command Code 账号并获得 New API 额度。贡献走各平台自己的网页授权，贡献者不需要交出原始凭据。
- **网页授权**：`POST /api/donations` 创建账号并返回需要打开的登录地址；`GET /api/donations/sessions/{id}` 轮询进度，登录完成后发放额度；`DELETE` 放弃本次贡献。账号未授权前不会发放额度。
- 贡献的账号先以禁用状态创建，授权成功后才启用，因此未完成的流程不会承载流量。
- 若授权成功但发放额度失败，账号保留在账号池中，响应返回 `credited:false` 与 `credit_error`，管理员无需让用户重新登录即可补发。
- 不支持网页授权的平台保留 `POST /api/donations/credential` 的粘贴凭据方式。
- 在「系统设置」里用 `donation_base_url` 和 `donation_token` 配置贡献站点。令牌以密钥形式保存，系统设置接口不会返回它。
