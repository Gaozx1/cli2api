### English

- A contributed account that needs a browser login now keeps that login across a restart. A child-process provider (Qoder) keeps its authorized login in a home directory that lives on tmpfs, so it is wiped on every restart; the gateway restores it from the database at startup, which means a login that was never stored is simply gone. The donation flow authorized the account and never saved the login, so the account served chat and then came back as "needs account login" after the next restart. Completing a contribution now persists the login, the same way the console's own add-account wizard already did. Accounts contributed before this fix cannot be recovered automatically — the login only ever existed in the wiped directory — so those need to be contributed again.

### 中文

- 通过贡献得到、需要网页登录的账号，现在**重启后仍保留登录状态**。子进程型 provider（Qoder）把已授权的登录放在 **tmpfs** 上的 home 目录里，每次重启都会被清空；网关启动时会从数据库恢复，因此**从未存入数据库的登录就直接丢失**。贡献流程授权成功后没有保存登录，于是账号能正常对话，但下次重启后就变成"需要账号登录"。现在贡献完成时会保存登录，与控制台自带的添加账号向导行为一致。**在此修复之前贡献的账号无法自动恢复**——那次登录只存在于已被清空的目录里——需要重新贡献一次。