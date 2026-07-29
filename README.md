# hrm-sql-mcp

給 AI agent 用的、有護欄的 SQL Server 存取層。同一份實作提供兩個介面：
MCP 伺服器（stdio，給 Claude Code 與 Gemini CLI）與 CLI（給人和 CI）。

**目前是唯讀的。** 寫入路徑（Phase 3）還沒有實作。

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
若用計劃書原本寫的 `--exclude-tools`（拒絕清單），Phase 3 新增的寫入工具會在
沒有人更新這行指令的情況下，自動對 Gemini 開放。差別只在有人忘記時會發生什麼。

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
hrm-sql-mcp sp audit --target hrm_0209 --format markdown --out "Stored Procedure/INVENTORY.md"
```

⚠ **旗標一律放在位置參數前面。** Go 的 flag 遇到第一個非旗標就停止解析，所以
`schema 特休 --limit 6` 會去搜尋字面字串 `"特休 --limit 6"`。程式偵測到這種寫法
會直接報錯，不會安靜地給你錯答案。

宣告多個目標時 `--target` 是必填的，工具不會替你猜。

---

## 環境變數

| 變數 | 說明 |
| :--- | :--- |
| `HRM_SQL_MCP_PROFILE` | **必填，沒有預設值**。`local` 或 `uat`。要求寫出來，是為了讓「指向某個環境」永遠是刻意的行為 |
| `HRM_SQL_MCP_POLICY` | 政策檔，預設 `mcp/hrm-sql.yaml`（相對於工作目錄）|
| `HRM_SQL_MCP_CREDENTIALS` | 0600 憑證檔，預設 `~/.config/hrm-sql-mcp/credentials.env` |
| `HRM_SQL_MCP_PROJECT_ROOT` | 解析政策檔裡的相對路徑，預設 `.` |
| `HRM_SQL_MCP_ACTOR` | 誰在操作，寫進每一行稽核。行程結束後 PID 就沒有意義了 |

政策檔宣告的 `profile` 必須與 `HRM_SQL_MCP_PROFILE` 相符，不符就拒絕啟動。
一個政策檔一個 profile——這是「我以為我在 local」不會變成一次 UAT 寫入的地方。

---

## 納管 `.mcp.json` 的取捨

`HRM/.mcp.json` 進版控，代表**任何 clone 這個 repo 的人都會自動獲得一個
資料庫工具**。緩解手段有三層：憑證檔不在 repo 裡且必須 0600、Claude Code 對
專案層 MCP 伺服器會要求核可、以及 SQL 登入本身的權限。但這是刻意的取捨，
不是疏忽——寫在這裡是為了讓它是個決定，而不是一個沒人注意到的預設。

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
- 稽核紀錄在操作**完成後**才寫，行程中途被 kill 就沒有那一行。全唯讀階段可接受；
  Phase 3 的寫入路徑必須先記意圖再動手。
