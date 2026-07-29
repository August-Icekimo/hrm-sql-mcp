# hrm-sql-mcp

給 AI agent 用的、有護欄的 SQL Server 存取層。同一份實作提供兩個介面：
MCP 伺服器（stdio，給 Claude Code 與 Gemini CLI）與 CLI（給人和 CI）。

**大部分操作是唯讀的。** 寫入要走核可閘門，見〈寫入〉一節。

---

## 這個工具的設計前提

**「不會連到正式機」由型別系統保證，不是靠註解拜託。** `target.Target` 沒有
匯出欄位也沒有匯出建構子，取得它的唯一路徑是 `Registry.Open`，而 `Open` 一定
先跑守衛。沒有任何程式碼路徑可以繞過。守衛每一步都 fail closed：無法解析的
主機名、空的 allow_cidrs、缺少的 profile，全部是拒絕，不是放行。

**擋寫入的是資料庫權限，不是語句分析。** T-SQL 的 `EXEC(@sql)` 讓任何靜態分析
都能被繞過；用正則做授權是丟掉薪資表的方式。真正的邊界是 `hrm_mcp_ro` 的
`DENY EXECUTE` 與沒有 writer role。這裡刻意**不做**用戶端語句分類器——真的哪天
DENY 掉了，分類器會代為擋下，而「這個帳號不能寫」的測試就會因為錯的理由通過。

**稽核沒有關閉開關。** 每次操作都寫一行 append-only JSONL，包含被守衛拒絕的
嘗試。政策檔的 `audit.file` 留空是套用預設值，不是停用。

---

## 安裝

```bash
go build -o ~/.local/bin/hrm-sql-mcp ./cmd/hrm-sql-mcp
```

裝在 PATH 上（不是寫死絕對路徑），註冊檔才不必綁定某一台機器的家目錄。

### 憑證

住在 `~/.config/hrm-sql-mcp/credentials.env`，**必須 0600**，程式會檢查並在
權限太寬時拒絕啟動。這個檔案不在任何 repo 裡。

```
HRM_SQL_<KEY>_RO_USER=...
HRM_SQL_<KEY>_RO_PASSWORD=...
HRM_SQL_<KEY>_RW_USER=...
HRM_SQL_<KEY>_RW_PASSWORD=...
```

`<KEY>` 是政策檔裡 target 的 `credential_key`（預設等於別名）大寫。多個目標
共用一組登入時就指向同一個 key——同一台伺服器的三份快照不需要把密碼抄三遍。

同一組 key 也可以直接設成環境變數，這時就不需要這個檔案；詳見〈連線組態〉。

**絕不重用應用程式的資料庫帳號。** 那會讓 DBA 端的歸因變成不可能，而且會繼承
應用程式的全部權限。

---

## 在新機器上接起來

### Claude Code

`HRM/.mcp.json` 已納管，clone 下來就有，不需要任何指令。

### Gemini CLI

**每台機器要自己跑一次**（`HRM/.gemini/settings.json` 含個人偏好，不納管）：

```bash
cd /path/to/HRM
gemini mcp add hrm-sql hrm-sql-mcp \
  --scope project --transport stdio \
  -e HRM_SQL_MCP_PROFILE=local \
  -e HRM_SQL_MCP_POLICY=mcp/hrm-sql.yaml \
  -e HRM_SQL_MCP_CREDENTIALS="$HOME/.config/hrm-sql-mcp/credentials.env" \
  -e HRM_SQL_MCP_ACTOR=gemini \
  --include-tools mssql_targets --include-tools mssql_query --include-tools mssql_explain \
  --include-tools mssql_deps --include-tools mssql_schema_search --include-tools mssql_sp_list \
  --include-tools mssql_sp_get --include-tools mssql_sp_diff --include-tools mssql_sp_audit \
  --description "HRM 測試快照（唯讀）" \
  -- serve
```

⚠ **`--include-tools` 要寫成重複的旗標，不要寫成逗號串。** 寫成
`--include-tools a,b,c` 時 Gemini 會把整串當成**一個**工具名存進去，結果是
一個工具都對不上。實測 0.40.1 版如此。

⚠ **絕對不要加 `--trust`。** 那會讓所有工具呼叫免確認，等於把核可閘門整個拆掉。

用 `gemini mcp list` 確認（**需要 TTY**，導向管線時不會有輸出）：

```
✓ hrm-sql: hrm-sql-mcp serve (stdio) - Connected
```

#### 為什麼 Gemini 用允許清單，Claude Code 不用

`--include-tools` 是**允許清單**：沒列出來的工具一律看不到，包含以後才加進來的。
若用計劃書原本寫的 `--exclude-tools`（拒絕清單），後來新增的 `mssql_execute` 與
`mssql_sp_deploy` 就會在沒有人更新這行指令的情況下自動對 Gemini 開放。
這兩支現在真的存在了，而上面那行指令一個字都不用改——差別只在有人忘記時會發生什麼。

但兩者都**不是安全邊界**——client 端過濾擋的是「不小心叫到」，不是「刻意繞過」。
真正的邊界仍是 SQL 登入的權限。兩層都要，不能只靠這一層。

---

## 用 CLI

```bash
export HRM_SQL_MCP_PROFILE=local
export HRM_SQL_MCP_POLICY=mcp/hrm-sql.yaml

hrm-sql-mcp targets                      # 先跑這個：看清楚會碰到哪一份快照
hrm-sql-mcp query --target hrm_0209 "SELECT ..."
hrm-sql-mcp explain --target hrm_0209 "SELECT ..."   # 只編譯不執行
hrm-sql-mcp deps --target hrm_0209 sp_SRJ0300
hrm-sql-mcp schema 特休                   # 查資料字典，中文或識別字都可以
hrm-sql-mcp sp list --target hrm_0209
hrm-sql-mcp sp get --target hrm_0209 sp_SRJ0300
hrm-sql-mcp sp diff --target hrm_0209    # 原始檔 vs 資料庫
hrm-sql-mcp sp audit --target hrm_0209 --format markdown --out "Stored Procedure/INVENTORY.md"
hrm-sql-mcp sp audit --offline           # 不連資料庫，見〈接進 CI〉
```

⚠ **旗標一律放在位置參數前面。** Go 的 flag 遇到第一個非旗標就停止解析，所以
`schema 特休 --limit 6` 會去搜尋字面字串 `"特休 --limit 6"`。程式偵測到這種寫法
會直接報錯，不會安靜地給你錯答案。

宣告多個目標時 `--target` 是必填的，工具不會替你猜。

---

## 寫入

寫入不是「多帶一個旗標」，是一條四步的路，而且**每一步都設計成無法一個人在一個
終端機裡連續做完**：

```bash
# 1. 演練。不需要核可，因為它會 rollback。回報影響筆數但什麼都沒留下。
hrm-sql-mcp execute --target hrm_0209 "UPDATE ... WHERE ..."

# 2. 認真要做時加 --commit。這一步不會執行，只會回一個 approval id。
hrm-sql-mcp execute --target hrm_0209 --commit "UPDATE ... WHERE ..."

# 3. 換一個終端機（通常是換一個人）看完整語句再決定。
hrm-sql-mcp approve            # 列出待核可
hrm-sql-mcp approve <id>       # 印出完整語句，然後記錄決定
hrm-sql-mcp approve <id> --deny --reason "WHERE 條件涵蓋太廣"

# 4. 帶著核可再跑一次，語句必須一字不差。
hrm-sql-mcp execute --target hrm_0209 --commit --approval <id> "UPDATE ... WHERE ..."
```

核可綁的是語句的 SHA-256，不是「這個人可以寫」。改一個空白就會被 `ErrStatement`
擋下——這是為了讓「核可的內容」與「執行的內容」不可能不同，而不是為了刁難。
核可會過期。

`sp deploy` 走同一條路，並在覆蓋前把資料庫上的舊定義存進 snapshot 目錄。

**擋寫入的最終邊界仍然是 SQL 權限。** 以上全部是流程控制，不是授權；
`hrm_mcp_ro` 的 `DENY EXECUTE` 才是那道牆。

---

## 接進 CI

`sp audit` 有兩種跑法，差別不是詳細程度，是**它們回答不同的問題**：

| | 比對來源 | 需要什麼 | 可重現？ |
| :--- | :--- | :--- | :--- |
| 完整 | 原始檔 × 資料庫 × Java | 連得到快照＋憑證 | ❌ 內容取決於連哪一份快照 |
| `--offline` | 原始檔 × Java | 什麼都不用 | ✅ 同一個 commit 到處都一樣 |

**只有離線那份能拿來擋關。** 完整報告的內容取決於你連的是 `hrm_0209`（252 支）
還是 `hrm_0424`（259 支），拿它做一致性檢查，只會讓連了另一份快照的同事無故被擋。
離線報告純粹由 repo 決定，所以「這份檔案過期了」是一個所有人都會得到相同答案的問題。

離線模式**不讀憑證檔**（`service.NewOffline`），所以在一台從來沒拿過資料庫密碼的
機器上也跑得起來——這正是它存在的理由。

### 退出碼

| 碼 | 意思 | 誰會用 |
| ---: | :--- | :--- |
| 0 | 正常 | |
| 1 | 工具本身出錯（設定錯、政策檔不見） | 要修，但不該擋 push |
| 2 | `--gate`：盤點有需要處理的項目 | 手動或有資料庫的 CI |
| 3 | `--check`：納管的報告與現況不符 | pre-push hook |

**`--gate` 不適合當 pre-push 的預設。** HRM 現在就有 36 支 `missing-script`，
用 `--gate` 會每一次都失敗，而永遠失敗的 hook 就是第一天被 `--no-verify` 繞過、
之後對誰都不再擋任何東西的 hook。`--check` 才是對的機制：既有的 36 支記在納管的
報告裡，第 37 支出現時檔案才會對不上。

### 在 HRM 裡怎麼接

```bash
scripts/sp-audit.sh --install-hook   # 每台機器各跑一次，.git/hooks 不在版控裡
scripts/sp-audit.sh                  # 離線檢查（hook 跑的就是這個）
scripts/sp-audit.sh --write          # 重新產生兩份報告，讀過 diff 再 commit
```

hook 在**找不到 `hrm-sql-mcp` 時直接跳過**，不是擋下 push。這個工具是個人裝的，
不是 HRM 的建置相依；因為同事沒裝 Go 工具而失敗的 hook，同樣會走上被繞過的路。

---

## 環境變數

| 變數 | 說明 |
| :--- | :--- |
| `HRM_SQL_MCP_PROFILE` | **必填，沒有預設值**。`local` 或 `uat`。要求寫出來，是為了讓「指向某個環境」永遠是刻意的行為 |
| `HRM_SQL_MCP_POLICY` | 政策檔，預設 `mcp/hrm-sql.yaml`（相對於工作目錄）|
| `HRM_SQL_MCP_CREDENTIALS` | 0600 憑證檔，預設 `~/.config/hrm-sql-mcp/credentials.env` |
| `HRM_SQL_MCP_PROJECT_ROOT` | 解析政策檔裡的相對路徑，預設 `.` |
| `HRM_SQL_MCP_ACTOR` | 誰在操作，寫進每一行稽核。行程結束後 PID 就沒有意義了 |
| `HRM_SQL_MCP_ENV_FILE` | 額外的 .env 檔，逗號分隔。見〈連線組態〉|

政策檔宣告的 `profile` 必須與 `HRM_SQL_MCP_PROFILE` 相符，不符就拒絕啟動。
一個政策檔一個 profile——這是「我以為我在 local」不會變成一次 UAT 寫入的地方。

---

## 連線組態

政策檔是基準；環境變數與 .env 檔可以蓋掉它的任何一個欄位。優先序由高而低：

```
行程環境變數  >  .env 檔（HRM_SQL_MCP_ENV_FILE，先列的先贏）  >  credentials.env  >  政策檔
```

### 覆寫既有目標

`HRM_SQL_TARGET_<別名大寫>_<欄位>`，別名裡的非英數字元換成底線：

| 欄位 | 例 |
| :--- | :--- |
| `HOST` | `HRM_SQL_TARGET_HRM_0209_HOST=127.0.0.1` |
| `PORT` | `HRM_SQL_TARGET_HRM_0209_PORT=14330` |
| `DATABASE` | `HRM_SQL_TARGET_HRM_0209_DATABASE=hrm_0511` |
| `ALLOW_CIDRS` | 逗號分隔；**設成空字串是拒絕全部**，不是解除限制 |
| `ENCRYPT` / `TRUST_SERVER_CERTIFICATE` / `WRITABLE` | `true` / `false` |
| `APP_NAME` / `CREDENTIAL_KEY` | 字串 |

### 新增政策檔沒有的目標

```bash
export HRM_SQL_MCP_EXTRA_TARGETS=hrm_0730
export HRM_SQL_TARGET_HRM_0730_HOST=127.0.0.1
export HRM_SQL_TARGET_HRM_0730_DATABASE=hrm_0730
export HRM_SQL_TARGET_HRM_0730_ALLOW_CIDRS=127.0.0.0/8
```

`HOST` / `DATABASE` / `ALLOW_CIDRS` 三個都是必填。**新目標不繼承任何人的允許網段**——
繼承來的網段等於沒有人替這個端點決定過爆炸半徑。

### 憑證

同一組 key 在環境變數與 .env 檔裡通用，所以從筆電搬到 CI 是「換個地方設」，不是「換個名字」：

```
HRM_SQL_<CREDENTIAL_KEY 大寫>_RO_USER / _RO_PASSWORD
HRM_SQL_<CREDENTIAL_KEY 大寫>_RW_USER / _RW_PASSWORD
```

憑證檔**不存在不再是致命錯誤**——沒有它時就從環境變數找。真的找不到才失敗，
而且是在用到的當下失敗，錯誤訊息會講它找的是哪一個 credential key。

⚠ **環境變數沒有 0600 這道保護。** 同一使用者的任何行程都讀得到 `/proc/<pid>/environ`，
子行程會繼承，CI 也常常把環境變數印進 log。**筆電上請繼續用檔案**；環境變數是為了
容器與 CI 那些放不了檔案的地方而開的，不是更方便的預設。

### .env 檔的權限規則由內容決定，不是由檔名

只要檔案裡出現任何 `*_PASSWORD`，就**必須是 0600**，不管它叫什麼名字。
只有主機名、資料庫名這類設定的檔案，0644 沒問題。用檔名判斷會被改名繞過。

### 這些覆寫碰不到的東西

`internal/constants` 的正式機黑名單是**編譯期**的，不是組態。把 `allow_cidrs`
用環境變數設成 `0.0.0.0/0`，只是放寬了這份政策自己的允許清單——守衛取兩者交集，
而被拒絕的那一側不是設定。實測：

```console
$ HRM_SQL_TARGET_HRM_0209_HOST=172.16.3.34 \
  HRM_SQL_TARGET_HRM_0209_ALLOW_CIDRS=0.0.0.0/0 hrm-sql-mcp targets
hrm_0209  -  -  -  REJECTED  target "hrm_0209" rejected at host: "172.16.3.34" is on the permanent host denylist
```

`internal/target/override_guard_test.go` 用六種敵意設定把這件事釘住。**那個測試掛掉，
代表覆寫功能吃掉了守衛，要回退而不是修補。**

### 看目前實際生效的組態

```bash
hrm-sql-mcp targets
```

多了 `LOGIN` 欄（憑證從哪來，或 `MISSING`），而且**只要有任何覆寫就一定會列出來，
沒有關閉開關**。當 host 與 database 可以來自三個地方，只顯示結果的清單答得出
「會連到哪」，答不出「為什麼」，而半夜要查的是後者。

---

## 納管 `.mcp.json` 的取捨

`HRM/.mcp.json` 進版控，代表**任何 clone 這個 repo 的人都會自動獲得一個
可寫的資料庫工具**——它沒有工具過濾，`mssql_execute` 與 `mssql_sp_deploy` 都在裡面。
緩解手段有四層：憑證檔不在 repo 裡且必須 0600、Claude Code 對專案層 MCP 伺服器
會要求核可、寫入要跨行程核可且綁語句雜湊、以及 SQL 登入本身的權限。
但這是刻意的取捨，不是疏忽——寫在這裡是為了讓它是個決定，而不是一個沒人注意到的預設。

---

## 測試

單元測試不需要資料庫。整合測試要本機容器，預設 skip：

```bash
go test ./...                                    # 離線部分
HRM_SQL_MCP_IT=1 \
HRM_PROJECT_ROOT=/path/to/HRM \
HRM_SP_DIR="/path/to/HRM/Stored Procedure" \
HRM_SCHEMA_DICT=/path/to/HRM/.gemini/database_schema.md \
  go test ./...                                  # 全部
```

整合測試會把稽核導到 `t.TempDir()`，不會汙染你真正的 audit.jsonl——那個檔案是
這個專案唯一當成證據看的東西。

---

## 已知限制

- **測試快照不是正式環境的完整還原**，彼此只差在還原時間與測試對象。任何盤點
  結論只對它跑的那一份成立，報告開頭會標明快照日期與程序數。
- **帶跨資料庫參照的程序在封閉容器裡建不起來**（HRM 的 WD 模組會連打卡系統
  SQLWD），所以它們在快照裡都會缺席。看 `sp audit` 的 ghost/db-only 要先排除這類。
- **`mssql_deps` 只看得到 SQL 對 SQL 的引用。** 動態組出來的名稱看不到，
  應用程式的呼叫點也不在裡面——那是 `mssql_sp_audit` 的 Java 掃描負責的。
- **資料字典有部分是錯的**，程式只回報不修正。細節見 `internal/schemadict` 的
  套件註解。
- **唯讀操作的稽核在完成後才寫**，行程中途被 kill 就沒有那一行。寫入路徑不同：
  它先記 `intent` 再動手，並且 fsync，所以「做了但沒記到」不會發生。
- **離線盤點答不出三件事**：哪些已部署、檔案是不是實際在跑的版本、有沒有無主的
  程序在線上。報告開頭會明講，但仍要提防把它的短清單讀成「沒問題」。
- **`--offline` 與 `--target` 互斥**，同時給會直接報錯。安靜地忽略其中一個，
  會讓 hook 以為它盤點了 `hrm_0209`，而它其實一個 socket 都沒開。
