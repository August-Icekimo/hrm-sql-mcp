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

預設住在 `~/.config/hrm-sql-mcp/credentials.env`。這個檔案不在任何 repo 裡。

```bash
CRED=~/.config/hrm-sql-mcp/credentials.env

mkdir -p "$(dirname "$CRED")"
touch "$CRED"          # 已存在就不動它——不要用 install 或 > 這類會清空的寫法
chmod 600 "$CRED"

cat >> "$CRED" <<'EOF'
HRM_SQL_LOCAL_HRM_RO_USER=hrm_mcp_ro
HRM_SQL_LOCAL_HRM_RO_PASSWORD=請填入
HRM_SQL_LOCAL_HRM_RW_USER=hrm_mcp_rw
HRM_SQL_LOCAL_HRM_RW_PASSWORD=請填入
EOF
```

`LOCAL_HRM` 是政策檔裡 target 的 `credential_key`（預設等於別名）**大寫**。多個
目標共用一組登入時就指向同一個 key——同一台伺服器的三份快照不需要把密碼抄三遍。

**只要檔案裡出現任何 `*_PASSWORD` 就必須是 0600**，程式會檢查並拒絕啟動。
規則看的是內容不是檔名，所以改名繞不過去。

這個檔案**不存在不是錯誤**——同一組 key 也可以直接設成環境變數，容器與 CI 就是
那樣給的。詳見〈連線組態〉。

**絕不重用應用程式的資料庫帳號。** 那會讓 DBA 端的歸因變成不可能，而且會繼承
應用程式的全部權限。

---

## 快速開始：先宣告，再做事

底下每一節的指令都假設你已經跑過這個區塊。

```bash
# ── 環境：貼一次就好，或放進 shell rc / direnv 的 .envrc ────────
export HRM_PROJECT="$HOME/eclipse-workspace/HRM_DEV/HRM"   # ← 改成你的路徑

export HRM_SQL_MCP_PROFILE=local                   # 必填，沒有預設值
export HRM_SQL_MCP_POLICY="$HRM_PROJECT/mcp/hrm-sql.yaml"
export HRM_SQL_MCP_PROJECT_ROOT="$HRM_PROJECT"
export HRM_SQL_MCP_ACTOR="${USER:-cli}"            # 寫進每一行稽核
```

```bash
# ── 這次工作要打哪一份快照：每個終端機各自決定，不要放進 rc ─────
TARGET=hrm_0209        # 三份：hrm_0209 / hrm_0424 / hrm_0511
```

`TARGET` 刻意不 export、也不建議寫進 shell rc——這個名字太通用（Makefile、
交叉編譯都在用），而且「這次要動哪一份資料」本來就該是每次刻意選的，
不是繼承來的預設。

⚠ **`HRM_SQL_MCP_POLICY` 是相對於「工作目錄」，不是 `PROJECT_ROOT`。**
兩者用途不同：`POLICY` 指政策檔本身，`PROJECT_ROOT` 是政策檔裡
`sp_dir` / `schema_dict` / `java_src_dir` 的解析基準。上面用絕對路徑寫
`POLICY`，就是為了讓你在任何目錄下都跑得起來；若寫成相對路徑，就必須先
`cd "$HRM_PROJECT"`，否則會得到 `open mcp/hrm-sql.yaml: no such file`。

```bash
# ── 然後做事 ───────────────────────────────────────────────────
hrm-sql-mcp targets                    # 一定先跑這個：看清楚會碰到哪一份快照
```

`targets` 會列出每個目標的伺服器、資料庫、守衛結果、憑證來源與快照日期，
並把所有非政策檔來源的覆寫一起印出來。連線出問題時，第一個要看的就是它。

---

## MCP 工具一覽

十一支，唯讀九支、寫入兩支。CLI 子指令與它們一一對應，走的是同一個
`internal/service`——所以稽核只有一個實作，不可能有哪個前端漏記。

| 工具 | CLI | 讀/寫 |
| :--- | :--- | :--- |
| `mssql_targets` | `targets` | 讀 |
| `mssql_query` | `query` | 讀 |
| `mssql_explain` | `explain` | 讀 |
| `mssql_deps` | `deps` | 讀 |
| `mssql_schema_search` | `schema` | 讀（讀文件，不是伺服器）|
| `mssql_sp_list` | `sp list` | 讀 |
| `mssql_sp_get` | `sp get` | 讀 |
| `mssql_sp_diff` | `sp diff` | 讀 |
| `mssql_sp_audit` | `sp audit` | 讀 |
| `mssql_execute` | `execute` | **寫**，需核可 |
| `mssql_sp_deploy` | `sp deploy` | **寫**，需核可 |

寫入兩支由 `mcpserver.AddWriteTools` 單獨註冊，不在 `New()` 裡——「哪些工具能寫」
是某個呼叫點上有人做的決定，不是一份會自己長大的清單的屬性。

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

沿用〈快速開始〉宣告的 `$TARGET`。

```bash
# ── 查資料 ─────────────────────────────────────────────────────
hrm-sql-mcp query   --target "$TARGET" "SELECT TOP 10 emp_no, chg_type FROM EMP_CAREER_CHG"
hrm-sql-mcp query   --target "$TARGET" --format json "SELECT ..."
hrm-sql-mcp query   --target "$TARGET" --max-rows 20 --timeout 10s "SELECT ..."
hrm-sql-mcp explain --target "$TARGET" "SELECT ..."      # 只編譯，不執行

# 長 SQL 用 stdin，免去引號地獄
hrm-sql-mcp query --target "$TARGET" - <<'SQL'
SELECT TOP 5 emp_no, chg_type, job_code
FROM   EMP_CAREER_CHG
WHERE  emp_no LIKE '00%'
ORDER  BY emp_no
SQL

# ── 查結構 ─────────────────────────────────────────────────────
hrm-sql-mcp schema 特休                                  # 資料字典，中文或識別字皆可
hrm-sql-mcp schema --limit 6 特休                        # 旗標要放在前面，見下方警告
hrm-sql-mcp deps    --target "$TARGET" sp_SRJ0300        # 它引用誰、誰引用它

# ── 查 Stored Procedure ────────────────────────────────────────
hrm-sql-mcp sp list --target "$TARGET"
hrm-sql-mcp sp get  --target "$TARGET" sp_SRJ0300
hrm-sql-mcp sp diff --target "$TARGET"                   # 全部：原始檔 vs 資料庫
hrm-sql-mcp sp diff --target "$TARGET" sp_SRJ0300        # 只比對指定幾支

# ── 盤點 ───────────────────────────────────────────────────────
hrm-sql-mcp sp audit --target "$TARGET"                  # 三方比對，印在畫面上
hrm-sql-mcp sp audit --offline                           # 不連資料庫，見〈接進 CI〉
hrm-sql-mcp sp audit --target "$TARGET" --format markdown \
                     --out "$HRM_PROJECT/Stored Procedure/INVENTORY.md"
```

⚠ **旗標一律放在位置參數前面。** Go 的 flag 遇到第一個非旗標就停止解析，所以
`schema 特休 --limit 6` 會去搜尋字面字串 `"特休 --limit 6"`。程式偵測到這種寫法
會直接報錯，不會安靜地給你錯答案。

宣告多個目標時 `--target` 是必填的，工具不會替你猜。

### 換一份快照做同一件事

因為目標是變數，比對兩份快照只是換一個值：

```bash
for TARGET in hrm_0209 hrm_0424 hrm_0511; do
  echo "── $TARGET ──"
  hrm-sql-mcp query --target "$TARGET" \
    "SELECT COUNT(*) AS procs FROM sys.procedures"
done
```

**單一快照的結論會誤導。** 一支程序「不見了」，可能是缺陷，也可能只是那份快照
的時間比較早——並排跑過三份才分得出來。

---

## 寫入

寫入不是「多帶一個旗標」，是一條四步的路，而且**每一步都設計成無法一個人在一個
終端機裡連續做完**：

```bash
# ── 宣告：語句只寫一次，四個步驟共用同一份 ──────────────────────
#    這不只是為了少打字。核可綁的是語句的雜湊，四步之間差一個空白就會被擋，
#    所以「同一份變數」正是這條流程要的東西。
#    EXAMPLE_TABLE 是佔位字串，請換成你真的要動的表。第 1 步永遠會 rollback，
#    所以就算貼著跑也不會改到任何資料。
read -r -d '' SQL <<'EOSQL'
UPDATE EXAMPLE_TABLE SET some_col = N'測試' WHERE key_col = 'A001'
EOSQL

# ── 1. 演練。不需要核可，因為它會 rollback；回報影響筆數但什麼都沒留下。
hrm-sql-mcp execute --target "$TARGET" "$SQL"

# ── 2. 認真要做時加 --commit。這一步仍然不會執行，只回一個 approval id。
hrm-sql-mcp execute --target "$TARGET" --commit "$SQL"
APPROVAL=<把上一步印出來的 id 填進來>

# ── 3. 換一個終端機（通常是換一個人）看完整語句再決定。
hrm-sql-mcp approve                                        # 列出待核可
hrm-sql-mcp approve "$APPROVAL"                            # 印出完整語句並核可
hrm-sql-mcp approve "$APPROVAL" --deny --reason "WHERE 條件涵蓋太廣"

# ── 4. 帶著核可再跑一次，語句必須一字不差。
hrm-sql-mcp execute --target "$TARGET" --commit --approval "$APPROVAL" "$SQL"
```

`sp deploy` 走同一條路，定義用位置參數或 `-`（stdin）給：

```bash
SP=sp_SRJ0300
SP_FILE="$HRM_PROJECT/Stored Procedure/$SP.sql"

# 演練。--name 是必填的：沒有名稱就無法在覆蓋前存下舊定義。
iconv -f UTF-16LE -t UTF-8 "$SP_FILE" | hrm-sql-mcp sp deploy --target "$TARGET" --name "$SP" -

# 認真要做時加 --commit，拿到 approval id 後照上面第 3、4 步走。
iconv -f UTF-16LE -t UTF-8 "$SP_FILE" |
  hrm-sql-mcp sp deploy --target "$TARGET" --name "$SP" --commit --reason "修正年資計算" -
```

⚠ **`Stored Procedure/` 底下大多是 UTF-16LE，直接 `cat` 進去會變亂碼。**
上面用 `iconv` 是因為 CLI 的 stdin 當成 UTF-8 讀。（`sp diff` / `sp audit`
不受影響——它們走的是內建解碼器，會自己判斷編碼並回報用了哪一種。）

用 `file -b "$SP_FILE"` 可以先確認該檔的編碼；HRM 實測是 105 個 UTF-16LE、
14 個 UTF-8、29 個 CP950。

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
cd "$HRM_PROJECT"

scripts/sp-audit.sh --install-hook   # 每台機器各跑一次，.git/hooks 不在版控裡
scripts/sp-audit.sh                  # 離線檢查（hook 跑的就是這個）
scripts/sp-audit.sh --write          # 重新產生兩份報告，讀過 diff 再 commit
scripts/sp-audit.sh --full           # 完整三方盤點，印在畫面上
```

腳本自己會設好所有 `HRM_SQL_MCP_*`，不需要先跑〈快速開始〉的宣告。
要改它連的快照：`HRM_SP_AUDIT_TARGET=hrm_0424 scripts/sp-audit.sh --full`。

### 直接用 CLI 當閘門（不透過腳本）

```bash
cd "$HRM_PROJECT"
REPORT="Stored Procedure/INVENTORY.offline.md"

# 檢查納管的報告是否過期；過期就 exit 3
hrm-sql-mcp sp audit --offline --check --format markdown --out "$REPORT"

# 有資料庫的環境才做得到：盤點結果有問題就 exit 2
hrm-sql-mcp sp audit --target "$TARGET" --gate
```

hook 在**找不到 `hrm-sql-mcp` 時直接跳過**，不是擋下 push。這個工具是個人裝的，
不是 HRM 的建置相依；因為同事沒裝 Go 工具而失敗的 hook，同樣會走上被繞過的路。

---

## 環境變數

| 變數 | 說明 |
| :--- | :--- |
| `HRM_SQL_MCP_PROFILE` | **必填，沒有預設值**。`local` 或 `uat`。要求寫出來，是為了讓「指向某個環境」永遠是刻意的行為 |
| `HRM_SQL_MCP_POLICY` | 政策檔，預設 `mcp/hrm-sql.yaml`。**相對於工作目錄**，不是 `PROJECT_ROOT` |
| `HRM_SQL_MCP_PROJECT_ROOT` | 政策檔裡 `sp_dir` / `schema_dict` / `java_src_dir` 的解析基準，預設 `.` |
| `HRM_SQL_MCP_CREDENTIALS` | 憑證檔，預設 `~/.config/hrm-sql-mcp/credentials.env`。**不存在不是錯誤** |
| `HRM_SQL_MCP_ENV_FILE` | 額外的 .env 檔，逗號分隔，優先序低於行程環境變數 |
| `HRM_SQL_MCP_ACTOR` | 誰在操作，寫進每一行稽核，預設 `cli`。行程結束後 PID 就沒有意義了 |
| `HRM_SQL_MCP_EXTRA_TARGETS` | 宣告政策檔沒有的目標，逗號分隔。見〈連線組態〉|
| `HRM_SQL_TARGET_<別名>_<欄位>` | 覆寫單一目標的任一欄位。見〈連線組態〉|
| `HRM_SQL_<KEY>_{RO,RW}_{USER,PASSWORD}` | 登入。同名 key 在 .env 檔與環境變數通用 |
| `HRM_SQL_MCP_IT` | 設為 `1` 才會跑整合測試，見〈測試〉|

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

單次覆寫不必 `export`，寫在指令前面就好：

```bash
HRM_SQL_TARGET_HRM_0209_DATABASE=hrm_0511 hrm-sql-mcp sp list --target hrm_0209
```

### 新增政策檔沒有的目標

還原了一份新快照，不想動已納管的政策檔時：

```bash
# ── 宣告新目標 ─────────────────────────────────────────────────
NEW=hrm_0730                       # 別名，大寫後就是變數名的一部分
export HRM_SQL_MCP_EXTRA_TARGETS="$NEW"
export HRM_SQL_TARGET_HRM_0730_HOST=127.0.0.1
export HRM_SQL_TARGET_HRM_0730_DATABASE=hrm_0730
export HRM_SQL_TARGET_HRM_0730_ALLOW_CIDRS=127.0.0.0/8
export HRM_SQL_TARGET_HRM_0730_CREDENTIAL_KEY=local_hrm            # 沿用同一組登入
export HRM_SQL_TARGET_HRM_0730_TRUST_SERVER_CERTIFICATE=true       # 本機容器是自簽憑證

# ── 確認它真的被認得、守衛放行、憑證也對上 ──────────────────────
hrm-sql-mcp targets

# ── 然後就跟其他目標一樣用 ──────────────────────────────────────
hrm-sql-mcp sp audit --target "$NEW"
```

`HOST` / `DATABASE` / `ALLOW_CIDRS` 三個都是必填。**新目標不繼承任何人的允許網段**——
繼承來的網段等於沒有人替這個端點決定過爆炸半徑。

⚠ **`TRUST_SERVER_CERTIFICATE` 也不繼承，預設是 `false`。** 少了它連本機容器會
得到 `TLS Handshake failed: failed to verify certificate`。政策檔裡那三個目標
各自寫了這一行，環境變數新增的目標不會自動拿到。
這個預設是刻意的——它等於「有加密但不驗證伺服器身分」，也就是沒有 MITM 防護，
在自簽憑證的本機容器可以接受，換到真的伺服器要重新評估。

`CREDENTIAL_KEY` 沒給時預設等於別名，也就是會去找 `HRM_SQL_HRM_0730_RO_*`。
上面指到 `local_hrm`，是為了沿用同一台伺服器既有的那組登入。

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

### 完整範例：容器 / CI 裡不用任何檔案

```bash
# ── 宣告（秘密由 CI 的 secret store 注入，不落地成檔案）──────────
export HRM_PROJECT=/workspace/HRM
export HRM_SQL_MCP_PROFILE=local
export HRM_SQL_MCP_POLICY="$HRM_PROJECT/mcp/hrm-sql.yaml"
export HRM_SQL_MCP_PROJECT_ROOT="$HRM_PROJECT"
export HRM_SQL_MCP_ACTOR=ci

export HRM_SQL_TARGET_HRM_0209_HOST=mssql        # 容器網路裡的服務名
export HRM_SQL_LOCAL_HRM_RO_USER=hrm_mcp_ro
export HRM_SQL_LOCAL_HRM_RO_PASSWORD="$CI_SECRET_RO_PASSWORD"

# ── 確認組態解析成你以為的樣子，再往下跑 ────────────────────────
hrm-sql-mcp targets                  # LOGIN 欄應為 env，不是 MISSING

# ── 任務 ───────────────────────────────────────────────────────
hrm-sql-mcp sp audit --target hrm_0209 --gate
```

完全連不到資料庫的 runner 上，把最後一行換成離線車道即可，前面那兩個
`HRM_SQL_*` 秘密也都不需要：

```bash
cd "$HRM_PROJECT"
hrm-sql-mcp sp audit --offline --check \
  --format markdown --out "Stored Procedure/INVENTORY.offline.md"
```

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
go test ./...                        # 離線部分，不需要任何設定

# ── 宣告整合測試要用的路徑（注意：這組變數名與 HRM_SQL_MCP_* 不同）─────
export HRM_SQL_MCP_IT=1
export HRM_PROJECT_ROOT="$HRM_PROJECT"
export HRM_SP_DIR="$HRM_PROJECT/Stored Procedure"
export HRM_SCHEMA_DICT="$HRM_PROJECT/.gemini/database_schema.md"

go test -count=1 ./...               # 全部；-count=1 避免吃到快取
go test -count=1 -run Override ./internal/target/   # 只跑守衛那組
```

⚠ 整合測試需要本機容器開著：`podman start HRM_TEST_0205`。沒設 `HRM_SQL_MCP_IT`
時它們會 **skip 而不是失敗**，所以看到「全綠」要先確認自己真的有設。

整合測試會把稽核導到 `t.TempDir()`，不會汙染你真正的 audit.jsonl——那個檔案是
這個專案唯一當成證據看的東西。

---

## 已知限制

- **測試快照不是正式環境的完整還原**，彼此只差在還原時間與測試對象。任何盤點
  結論只對它跑的那一份成立，報告開頭會標明快照日期與程序數。
- **帶四段式 linked server 參照的程序在封閉容器裡建不起來**，所以它們在快照裡
  必然缺席。看 `sp audit` 的 ghost/db-only 要先排除這一類。
  實例：`sp_WDC0100` 參照 `CEPPHR.CEPPHR.dbo.tblMember`，而容器的 `sys.servers`
  裡沒有 `CEPPHR`，`CREATE PROCEDURE` 會以 Msg 7202 失敗。
  用 `hrm-sql-mcp query --target "$TARGET" "SELECT name FROM sys.servers"`
  可以看容器認得哪些伺服器。
  ⚠ 這類程序在**正式環境**是否存在，從測試快照判斷不出來——快照建不起它們，
  「快照沒有」因此不帶任何資訊。要請 DBA 對正式環境查。
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
